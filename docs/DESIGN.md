# `migration` — Design Document

## 1. Background

Two S3-compatible storage clusters (RustFS) run side by side in production:

- **Old** — single-node RustFS instance, holding historical data.
- **New** — multi-node RustFS cluster fronted by HAProxy, receiving all new writes.

The application layer is already dual-homed:

- **Reads and deletes** fan out to both clusters; whichever responds wins.
- **Writes** go **only** to New.

Historical objects must be migrated from Old into New so that Old can eventually
be decommissioned. The migration must coexist with live traffic and must not
compromise either cluster's correctness.

## 2. Goals and non-goals

### 2.1 Goals

1. Transfer every existing object on Old into New, **unless** New already has an
   object with the same key.
2. Be **safe**: never overwrite, never delete, never disturb live traffic.
3. Be **resumable**: any restart or crash must not redo finished work and must
   not leave New in an inconsistent state.
4. Be **scalable**: handle hundreds of millions of objects and multi-gigabyte
   files without OOM.
5. Be **observable**: clear progress output, an auditable conflict log, and a
   queryable local state.

### 2.2 Non-goals

- Two-way sync, schema/version migration, bucket-policy migration.
- Coordinating with the application — its dual-read / write-to-new behaviour is
  assumed.
- Cross-region migration or re-encryption.

## 3. Hard constraints

| Constraint                                                  | Rationale                                                          |
| ----------------------------------------------------------- | ------------------------------------------------------------------ |
| **No** `CopyObject` API                                     | Old and New are disjoint clusters; server-side copy is impossible. |
| **No** `DeleteObject` on either cluster                     | Application semantics require Old's data to remain readable until migration is complete. |
| **Never** overwrite an existing object on New               | New is the source of truth for live writes; overwriting would clobber fresher data. |
| **Tolerate** source-side deletes during migration           | The application may legitimately delete a key on Old mid-run.      |
| **Bounded memory**                                          | Object streams can be arbitrarily large.                           |
| **Resumable** after `SIGINT` / `SIGTERM`                    | Migration may span hours or days.                                  |

## 4. High-level architecture

```
┌───────────────┐      scan (read-only)       ┌──────────────────┐
│  Old (RustFS) │ ◀────────────────────────── │     migration     │
└───────────────┘                             │                  │
                                              │  ┌──────────┐    │
                                              │  │ SQLite   │    │ ← migration.db (WAL)
                                              │  │ (WAL)    │    │
                                              │  └──────────┘    │
┌───────────────┐    migrate (HEAD + GET/PUT) │                  │
│  New (RustFS) │ ◀────────────────────────── │   conflict.log   │
│  via HAProxy  │                             └──────────────────┘
└───────────────┘
```

Commands:

1. **`list`** — list buckets on the source (read-only).
2. **`scan`** — pure read against Old, dump inventory into SQLite.
3. **`migrate`** — workers consume `pending` / `failed` rows, perform
   HEAD / GET / PUT, update state.
4. **`run`** — `scan` then `migrate` (recommended one-step path).
5. **`status`** — print aggregate counters from the local DB(s).
6. **`diff`** — list source + HEAD target; report missing / size / ETag gaps.

`bucket:` empty means every source bucket (one SQLite file per bucket).
S3 `ListObjectsV2` has no LastModified server filter, so incremental runs
are a full list plus upsert of non-terminal rows — not a time window.

## 5. Component design

### 5.1 Module layout

```
cmd/                CLI (cobra), bilingual help, signal handling
internal/config/    YAML parsing, env-var overrides, defaults, validation
internal/s3client/  AWS SDK v2 client tailored for self-hosted S3
internal/cluster/   ListBuckets / HeadBucket / CreateBucket
internal/store/     SQLite persistence layer (keyset pagination)
internal/scanner/   ListObjectsV2 paging + batched inserts
internal/migrator/  Worker pool, copy pipeline, multipart, validation
internal/differ/    Source vs target audit
internal/fakes3/    httptest S3 stand-in for integration tests
```

The clean split between `scanner` and `migrator` keeps each phase replayable in
isolation and makes both testable without the other.

### 5.2 Configuration (`internal/config`)

- YAML is the primary configuration source.
- Environment variables override YAML when set: per-cluster `OLD_*` / `NEW_*`
  keys, the shared `REGION` fallback, then top-level `BUCKET`, `DB_PATH`,
  `CONFLICT_LOG`, `SPOOL_DIR`, `WORKERS`, `MULTIPART_CONCURRENCY`, and
  `RATE_LIMIT`.
- CLI flags win over both: `--bucket`, `--workers`, `--multipart-concurrency`,
  `--rate-limit`.
- Defaults are applied after env-var merging, so an omitted or zero
  `workers` / `multipart_concurrency` / `SPOOL_DIR` still collapses to the
  built-in default (`workers` 16, part concurrency 4, spool dir next to the
  DB). Negative concurrency or size values are rejected. An explicit `0` in
  `WORKERS`, `MULTIPART_CONCURRENCY`, or on the CLI is also rejected.
- `rate_limit` is a byte rate (`20M` = 20×1000×1000 B/s, `20MiB` =
  20×1024×1024 B/s, or a bare byte count). `0` or omit means unlimited at
  every layer, including the CLI and `RATE_LIMIT`. Negative values and
  unknown suffixes are rejected. This knob does not follow the concurrency
  rule where YAML `0` means "use the default".
- `bucket` may be empty (every source bucket). `list` / `scan` require the
  source endpoint and keys; `run` / `migrate` / `diff` also require the target.

### 5.3 S3 client (`internal/s3client`)

- Built from `aws-sdk-go-v2` with **static credentials** and **path-style
  addressing** (`UsePathStyle = true`), because self-hosted RustFS / MinIO /
  HAProxy deployments typically lack per-bucket DNS.
- The endpoint is fixed via `BaseEndpoint` rather than the deprecated
  `EndpointResolver`, so the SDK does not rewrite host names.
- Region is honoured but mostly cosmetic for self-hosted backends.
- One client per endpoint (Old, New); both are shared across all workers — the
  SDK's HTTP transport handles connection pooling internally.

### 5.4 Local store (`internal/store`)

Backed by **`modernc.org/sqlite`** (pure-Go SQLite, no CGO). Schema:

```sql
CREATE TABLE objects (
    key            TEXT PRIMARY KEY,
    size           INTEGER NOT NULL,
    last_modified  INTEGER NOT NULL,            -- unix seconds
    etag           TEXT,
    status         TEXT NOT NULL DEFAULT 'pending',
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    updated_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_objects_status ON objects(status);
```

Pragmas applied at open: `journal_mode=WAL`, `synchronous=NORMAL`,
`busy_timeout=5000ms`.

Per-object state machine:

```
        ┌──────────┐  (HEAD-New exists, size or comparable ETag differs)
        │ pending  │ ──────────────────────▶  conflict   (terminal)
        └────┬─────┘
             │  (GET-Old 404)
             ▼
        skipped (terminal)
             │
             │  (copy OK + verified, or HEAD-New already matches inventory)
             ▼
        done    (terminal)
             │
             │  (any other error)
             ▼
        failed
             │
             │  (manual re-run of `migrate`)
             ▼
        pending  (re-enter the loop)
```

`BatchUpsertScanned` uses `ON CONFLICT(key) DO UPDATE … WHERE status IN
('pending','failed','skipped')` and resets those rows to `pending`.
`done` / `conflict` stay terminal so a re-scan never overwrites finished work
or clobbers a live object on New. A `skipped` object that later reappears on
Old is retried.

### 5.5 Scanner (`internal/scanner`)

- Single goroutine driving `s3.NewListObjectsV2Paginator`. `workers` does
  not apply: each page needs the previous continuation token, and the rows
  land in one SQLite file.
- Accumulates objects into a 500-row buffer, flushed inside one transaction.
  This trades a tiny memory footprint for batched fsyncs.
- Honours `ctx.Done()` between pages so `SIGINT` during a scan is also
  graceful.

### 5.6 Migrator (`internal/migrator`)

#### Worker pool

- A buffered jobs channel of size `min(workers × 2, 2048)`.
- `workers` goroutines consume jobs.
- A feeder walks `BatchPendingAfter` (keyset pagination, 1000 rows) and
  pushes onto the channel, blocking on the cancellable context to
  back-pressure naturally.
- A 10-second ticker logs aggregate counters from `atomic.Int64` fields.
- Source body reads (the small-object spool and each multipart range) share
  one token bucket, `cfg.RateLimit` (`internal/ratelimit`). Nil / `0` is
  unlimited. Bytes are charged after they are pulled from the source, at
  most one ~100ms quantum (1KiB..32KiB) ahead of the wait, so the long-run
  sum across workers and parts stays at the cap. Uploads are not charged a
  second time. Context cancel unblocks a waiter and drops that reservation
  when it is still the tail of the timeline. The bucket only slows reads;
  it does not overwrite or delete. Buckets are copied one at a time, so
  the migrator's bucket is the process-wide cap.

#### Per-object pipeline (`handle`)

```
HEAD(New)
  ├─ 200 and matches inventory ─▶ mark done (already present), return
  ├─ 200 otherwise ────▶  write conflict.log, mark conflict, return
  ├─ 404      ──┐
  └─ other err └────▶   mark failed, return
                │
                ▼
         GetObject(Old)
                │
                ├─ NoSuchKey/404, request finished ─▶ log WARN, mark skipped, return
                ├─ cancel / deadline / request canceled ─▶ mark failed, return
                └─ other err      ─▶ mark failed, return
                │
                ▼
         size < threshold ?
            yes ──▶  copySingle  (spool to disk, then PutObject)
            no  ──▶  copyMultipart
                │
                ▼
         verify size (+ ETag for single-part)
                │
                ▼
         mark done
```

#### Small-object path (`copySingle`)

1. `GetObject(Old)` to obtain the body and source metadata.
2. Stream the body into `os.CreateTemp(cfg.SpoolDir, …)`. The SDK requires a
   seekable body for `PutObject` because it computes a payload hash; an
   in-memory buffer would defeat the bounded-memory goal for objects close to
   the multipart threshold.
3. `Seek(0)` and `PutObject(New)` with all preserved metadata.
4. `HeadObject(New)` to compare size and (when both sides expose a non-multipart
   ETag) ETag.

#### Large-object path (`copyMultipart`)

1. Compute `numParts = ceil(size / partSize)`. If it would exceed the S3 hard
   limit (10 000 parts), grow `partSize` so the upload fits.
2. `HeadObject(Old)` to capture common headers and user metadata, then
   `CreateMultipartUpload(New)` with those attributes.
3. Spawn one goroutine per part, gated by a semaphore of size
   `multipart_concurrency`. The acquire selects on `ctx.Done()`, so Ctrl-C
   does not block behind a full semaphore and does not schedule further
   parts. Each goroutine:
   - Issues a ranged `GetObject(Old, Range=bytes=start-end)`.
   - Reads exactly that range into memory (bounded by `partSize`).
   - Calls `UploadPart(New)` with explicit `PartNumber` and `ContentLength`.
   - Records the returned `ETag` at `parts[idx]`.
4. The first error wins (mutex-protected). On any failure or context
   cancellation, the upload session is cleaned up with `AbortMultipartUpload`
   using a fresh `context.WithTimeout(context.Background(), 30s)`, so cleanup
   still runs after the main migration context has been cancelled. **No
   `DeleteObject` is ever called** — `Abort` only releases the in-flight
   upload session, not any committed object.
5. On success: `CompleteMultipartUpload(New)` with the assembled
   `[]CompletedPart`.
6. A final `HeadObject(New)` compares `ContentLength` with the source size.

Preserved metadata on both paths: `Content-Type`, `Cache-Control`,
`Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, and
any `x-amz-meta-*` user metadata.

#### Status persistence and fatal-error handling

The worker's return value distinguishes two error classes:

- **Per-object errors** (transient 5xx, network blips, validation mismatch) →
  `markFailed` records the message and returns `nil`. The worker keeps going.
- **Fatal errors** (SQLite write failed, `conflict.log` append failed) → the
  worker returns the error, which is fed into `recordFatal`. The first fatal
  error is captured **and** triggers `cancel()` on the worker context, so the
  feeder stops pushing and other workers drain quickly instead of compounding
  failures into a broken state file. The first fatal error is what `Run`
  returns.

### 5.7 Conflict log (`conflictWriter`)

A mutex-guarded `os.File` opened with `O_APPEND`. Each conflict produces one
TSV line: `key \t old.LastModified \t new.LastModified` (RFC3339 UTC). Append
is crash-safe; restart simply continues appending.

### 5.8 CLI and signal handling (`cmd/root.go`)

- `cobra` provides subcommands and bilingual help.
- Persistent flags: `-c/--config`, `--bucket`, `--workers`,
  `--multipart-concurrency`, `--rate-limit`. Priority is CLI, then
  environment, then YAML, then the built-in default.
- `signalCtx` wires `SIGINT` / `SIGTERM` to context cancellation. The first
  signal cancels the worker pool (in-flight multipart uploads are aborted,
  not deleted); a second signal calls `os.Exit(130)`.

## 6. Concurrency and memory model

- Peak in-flight HTTP bodies are bounded by
  `workers × multipart_concurrency` (multipart concurrency is at least 1).
  Multipart memory is bounded by `partSize` per in-flight part. The job
  channel is capped (`JobBuffer`, at most 2048 descriptors) so a huge
  `--workers` cannot queue the whole bucket in memory. Small uploads spool
  through temporary files in `spool_dir` (default `filepath.Dir(db_path)`)
  so memory does not grow with object size below the multipart threshold.
  Set `spool_dir` to a real on-disk path when the host's `/tmp` is backed
  by tmpfs. More workers means more of those temp files at once.
- The first fatal persistence error cancels the worker context so the pool
  drains quickly. The error surfaced to the caller is still the first one
  observed; later cascading errors are swallowed.
- Each S3 client uses an `http.Transport` whose `MaxIdleConnsPerHost` follows
  `workers × multipart_concurrency` (capped). That replaces the Go default of
  2 idle connections per host so the worker pool can reuse connections. It
  is not a hard cap on in-flight requests; the worker pool and the part
  semaphore are.
- The state DB uses WAL and `SetMaxOpenConns(1)`. Workers still copy in
  parallel; only the status updates share one SQLite connection, which keeps
  a high `--workers` from turning the single-writer lock into
  `database is locked`.
- Shared mutable state is limited to:
  - Per-object SQLite rows (one connection, WAL),
  - Atomic counters (`atomic.Int64`),
  - The conflict-log mutex,
  - The mutex-protected `firstFatal` / `firstErr` slots inside `Run` and
    `copyMultipart`.

There is **no global lock on the copy path** — workers proceed
independently. Status rows share the single SQLite connection above.

## 7. Failure modes and responses

| Failure                                  | Detection                          | Response                                                                                |
| ---------------------------------------- | ---------------------------------- | --------------------------------------------------------------------------------------- |
| Old returns transient 5xx                | SDK error                          | Mark `failed`; retried on the next `migrate` run.                                       |
| Old returns 404 (`NoSuchKey`)            | `errors.As` on typed error         | Mark `skipped`; **not** treated as an error.                                            |
| Context canceled, deadline, or request canceled during Get/Upload, even if the text contains `NotFound` or `StatusCode: 404` | Checked before `IsNotFound` | Mark `failed`. A multipart abort's `NoSuchUpload` is not a missing source object. The next `migrate` retries. |
| New `HeadObject` returns 200 and matches inventory size and comparable ETag | nil error from HEAD | Mark `done`; do not write. Covers a PUT that landed before a cancelled `MarkStatus(done)`. |
| New `HeadObject` returns 200 but size or comparable ETag differs | nil error from HEAD | Conflict path; never write; append to `conflict.log`. |
| New `PutObject` / `UploadPart` fails     | SDK error                          | Abort any active multipart, mark `failed`, retry on next run.                           |
| SQLite or `conflict.log` write fails     | local I/O error                    | First fatal error wins; worker ctx cancelled; command exits non-zero.                   |
| Partial multipart on crash               | `migrate` re-run                   | Row is still `pending` / `failed`; new `CreateMultipartUpload` starts fresh; orphaned session is GC'd by RustFS lifecycle. |
| Network / HAProxy flake                  | SDK error                          | Mark `failed`; retried later.                                                           |
| `SIGINT` during heavy I/O                | `ctx.Done()`                       | First signal cancels gracefully; second signal forces exit (`130`).                     |
| Disk full on SQLite                      | SQLite error                       | Treated as fatal; operator must intervene before re-running.                            |
| Misconfigured credentials                | early SDK error                    | Command exits non-zero before any side effects on New.                                  |

## 8. Validation strategy

Two layers of validation are applied after each successful copy:

1. **Single-part**: a `HeadObject(New)` size check, plus a normalised-ETag
   comparison (`strings.Trim(s, "\"")`) when **both** sides report a
   non-multipart ETag (no `-N` suffix). RustFS, like S3, sets the ETag to the
   MD5 of the object for single-part PUTs.
2. **Multipart**: ETags between source and destination differ because they
   depend on part boundaries, so the verification falls back to a
   `HeadObject(New)` `ContentLength` check against the source size recorded
   at scan time.

If validation fails for an object, its row is marked `failed` with the
size / ETag mismatch in `last_error`.

## 9. Security considerations

- Credentials are read from local YAML and / or environment variables. The
  operator is responsible for protecting both (e.g. `chmod 600` for the YAML
  file, restricted process environments in production).
- Nothing in the code logs credential material. Endpoints and object keys are
  logged at INFO level, so operational logs should be treated accordingly.
- No outbound traffic occurs other than to the configured S3 endpoints.

## 10. Testing strategy

The repository ships with several test layers:

1. **Unit** — `internal/config`, `internal/store`, `internal/s3client`, `cmd` help text.
2. **Integration** — `internal/migrator` and `internal/differ` spin up two
   in-process fake S3 servers (`internal/fakes3`) that speak enough of the S3
   wire protocol to drive the SDK:
   - `TestScanAndMigrateEndToEnd`: conflict, source-404 skip, single-part and
     multipart, metadata, no `CopyObject` / `DeleteObject`.
   - `TestResumeAfterTransientFailure`: 5xx on `UploadPart`, resume without
     re-uploading completed objects.
   - `TestIdenticalTargetIsDoneDifferentContentConflicts`: identical target
     is `done`; same size with a different ETag, or a different size, stays
     `conflict` and is not overwritten.
   - `TestResumeAfterPutBeforeMarkDone`: PUT committed while the row is still
     `failed` resumes as `done`.
   - `TestSkippedObjectRescannedAndCopied`: skipped objects that reappear are
     retried.
   - `TestCreateMissingTargetBucket`: `CreateBucket` on an empty target.
   - `TestDiffReportsMissingAndExtra` / `TestDiffCleanWhenInSync`.
   - `TestListAndEnsureBucket`.

Recommended additional tests for production validation:

- A bake against a real MinIO / RustFS pair with a few hundred thousand
  objects of mixed sizes, including a multi-gigabyte object.
- A crash-recovery drill: `kill -9` the process mid-multipart, restart, and
  confirm zero duplicates and zero data loss via a separate audit script.

## 11. Future extensions

- **Pluggable state backends** (e.g. PostgreSQL) for scans coordinated across
  multiple migration hosts.
- **End-to-end server-side checksums** (`x-amz-checksum-*`) once both clusters
  consistently support them, so multipart uploads can verify content rather
  than only size.
- **Object-rate limiting** (objects/sec). Aggregate source-read bandwidth is
  already capped by `rate_limit` / `--rate-limit`.
- **Versioned buckets / delete markers / Object Lock** (explicitly out of
  scope today).
