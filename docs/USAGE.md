# `rustfs-migration` — Usage Guide

A command-line tool for **one-way, non-destructive** object migration between two
S3-compatible (RustFS) clusters.

The tool was built for the following topology, but works against any pair of
S3-compatible endpoints:

- **Old** — a single-node RustFS instance holding historical data.
- **New** — a multi-node RustFS cluster, typically fronted by HAProxy, that is
  already receiving all new application writes.

---

## 1. What it does in one sentence

For every object that exists on Old but not on New, copy it to New. Never
overwrite. Never delete. Resume cleanly after any interruption.

---

## 2. Prerequisites

| Item            | Requirement                                                                  |
| --------------- | ---------------------------------------------------------------------------- |
| Go toolchain    | 1.22+ (pure-Go SQLite, **no CGO required**)                                  |
| Network         | The host running the tool must reach both Old and New endpoints              |
| Credentials     | Access key / secret key per cluster: **read** on Old, **read+write** on New  |
| Local disk      | Free space for the SQLite state DB, the conflict log, and small-object spool |

---

## 3. Build

```bash
git clone <your-repo>/rustfs-migration.git
cd rustfs-migration
go mod tidy
CGO_ENABLED=0 go build -o bin/rustfs-migration .
```

The resulting binary is fully static. Common cross-compile targets:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/rustfs-migration-linux-amd64 .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/rustfs-migration-linux-arm64 .
```

---

## 4. Configuration

Copy the template and edit it:

```bash
cp config.example.yaml config.yaml
```

```yaml
old:
  endpoint:   "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region:     "us-east-1"

new:
  endpoint:   "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region:     "us-east-1"

bucket:       "my-bucket"
db_path:      "./migration.db"
conflict_log: "./conflict.log"
spool_dir:    ""              # small-object temp dir; defaults to dir(db_path)

workers:               16          # objects in flight concurrently
multipart_threshold:   52428800    # 50 MiB; objects ≥ this size use multipart
multipart_part_size:   16777216    # 16 MiB per part
multipart_concurrency: 4           # parts uploaded in parallel inside one object
```

### 4.1 Field reference

| Key                          | Required | Default            | Meaning                                                                       |
| ---------------------------- | -------- | ------------------ | ----------------------------------------------------------------------------- |
| `old.endpoint`               | yes      | —                  | URL of the Old cluster (path-style addressing is forced)                      |
| `old.access_key`, `secret_key` | yes    | —                  | Read credentials for Old                                                      |
| `new.endpoint`               | yes      | —                  | URL of the New cluster (HAProxy VIP works fine)                               |
| `new.access_key`, `secret_key` | yes    | —                  | Read+write credentials for New                                                |
| `bucket`                     | yes      | —                  | Bucket to migrate (same name on both sides). Must not contain whitespace or `/`. The tool calls `HeadBucket` before scanning and rejects any list page whose `Name` differs from this value. |
| `db_path`                    | no       | `./migration.db`   | Local SQLite database holding the scan inventory and per-object status. **Bound to `bucket` on first use** — reusing the same file for a different bucket fails fast. Use one `db_path` per bucket. |
| `conflict_log`               | no       | `./conflict.log`   | Append-only TSV log of keys that already exist on New                         |
| `spool_dir`                  | no       | `dir(db_path)`     | Directory where small objects are spooled before `PutObject`; set explicitly if the default points at tmpfs |
| `workers`                    | no       | `16`               | Worker goroutines processing objects in parallel                              |
| `multipart_threshold`        | no       | `50 MiB`           | Objects of this size or larger are uploaded via Multipart Upload              |
| `multipart_part_size`        | no       | `16 MiB`           | Size of each multipart part                                                   |
| `multipart_concurrency`      | no       | `4`                | Parts uploaded in parallel within a single large object                       |

> Effective peak fan-out ≈ `workers × multipart_concurrency`. Verify that
> HAProxy `maxconn` and the New cluster can absorb it.

### 4.2 Environment variables

Environment variables override the YAML when present. They are useful for
container deployments and secret managers.

| Variable                                              | Overrides                                       |
| ----------------------------------------------------- | ----------------------------------------------- |
| `OLD_ENDPOINT`, `OLD_AK`, `OLD_SK`, `OLD_REGION`      | Old endpoint, credentials, region               |
| `NEW_ENDPOINT`, `NEW_AK`, `NEW_SK`, `NEW_REGION`      | New endpoint, credentials, region               |
| `REGION`                                              | Shared region fallback when both per-cluster regions are empty |
| `BUCKET`, `DB_PATH`, `CONFLICT_LOG`, `SPOOL_DIR`      | Top-level runtime settings                      |

---

## 5. Commands

The CLI exposes three subcommands. The global `-c/--config` flag points to the
YAML file.

### 5.1 `scan` — inventory the Old bucket

```bash
./bin/rustfs-migration scan -c config.yaml
```

What it does:

1. Iterates the entire bucket on **Old** using `ListObjectsV2` with paging.
2. Buffers 500 results at a time and writes them into SQLite inside a single
   transaction, amortising fsync cost.
3. For each object records `{key, size, last_modified, etag, status=pending}`.

`scan` is **idempotent and safe to re-run**: the upsert only refreshes rows
whose status is `pending` or `failed`. Terminal rows (`done`, `conflict`,
`skipped`) are preserved, so running `scan` again after new objects appear on
Old will pick those up without disturbing prior migration results.

### 5.2 `migrate` — copy Old → New

```bash
./bin/rustfs-migration migrate -c config.yaml
```

What it does per object, inside a worker pool:

1. **`HeadObject(New)`** — if the key already exists on New:
   - Append `<key>\t<old.LastModified>\t<new.LastModified>` to `conflict.log`.
   - Mark the row `conflict` and move on. **The object on New is never touched.**
2. **`GetObject(Old)`** — if Old returns `NoSuchKey` / 404 (e.g. the application
   deleted the key after the scan), log a `WARN` and mark the row `skipped`.
   This is not treated as an error.
3. **Copy** (one of two paths):
   - **Small object** (`size < multipart_threshold`): stream the body to a
     temp file under `spool_dir`, then `PutObject(New)` from that file (the
     SDK needs a seekable body for payload hashing).
   - **Large object** (`size ≥ multipart_threshold`):
     `CreateMultipartUpload(New)` → ranged `GetObject(Old)` → `UploadPart(New)`
     for each part with bounded concurrency → `CompleteMultipartUpload(New)`.
     Any error triggers `AbortMultipartUpload` using a fresh 30-second timeout
     context, so cleanup runs even if the migration context was cancelled.
   - Headers and user metadata are preserved on both paths: `Content-Type`,
     `Cache-Control`, `Content-Disposition`, `Content-Encoding`,
     `Content-Language`, `Expires`, and any `x-amz-meta-*`.
4. **Verify**:
   - Small: `HeadObject(New)` and compare size; also compare normalized ETags
     when both sides report a non-multipart ETag (no `-N` suffix).
   - Large: `HeadObject(New)` and compare size only (multipart ETags depend on
     part boundaries and are not portable across clusters).
5. **Persist**: update the SQLite row to `done`, `failed`, `conflict` or
   `skipped`. If persistence itself fails, the worker reports it as a fatal
   error and the migration context is cancelled so the pool drains promptly.

`migrate` is **resumable**. It only iterates rows whose status is `pending`
or `failed`, so re-running after a crash, SIGINT, or partial failure resumes
exactly where it left off without re-uploading completed objects.

### 5.3 `status` — show progress

```bash
./bin/rustfs-migration status -c config.yaml
# total=12345 pending=10 done=12300 conflict=20 skipped=5 failed=10
```

---

## 6. Outputs

| File            | Purpose                                                                     |
| --------------- | --------------------------------------------------------------------------- |
| `migration.db`  | SQLite database; single source of truth for what was scanned and migrated   |
| `conflict.log`  | Append-only TSV log; one line per conflicting key                           |
| stdout / stderr | Structured `log` lines for progress, warnings, and errors                   |

`conflict.log` format (tab-separated, UTC RFC3339 timestamps):

```
<ObjectKey>\t<Old LastModified>\t<New LastModified>
```

---

## 7. Operational recipes

### 7.1 Pre-flight check (no destructive operation)

`scan` is read-only against Old; running it first and then `status` gives an
accurate count of objects that would be migrated, without writing anything to
New. If you want to additionally verify write credentials, run `migrate` for a
few seconds — it will skip every key that already exists on New and stop the
moment you `Ctrl-C`.

### 7.2 Graceful stop and resume

- The **first** `SIGINT` / `SIGTERM` cancels the worker context. In-flight
  uploads either finish their current part or are aborted via
  `AbortMultipartUpload`, and SQLite is flushed.
- A **second** signal calls `os.Exit(130)` to escape any stuck I/O.
- Just re-run `migrate` to continue.

### 7.3 Retrying failures

Failures are recorded with their last error message. Running `migrate` again
automatically retries every `pending` or `failed` row — no flag, no schema
change required.

### 7.4 Picking up objects written to Old after the initial scan

```bash
./bin/rustfs-migration scan    -c config.yaml   # inserts new keys as 'pending'
./bin/rustfs-migration migrate -c config.yaml   # migrates the new pendings
```

### 7.5 Tuning throughput

- Bottlenecked by small-object overhead → raise `workers`.
- Bottlenecked by single large files → raise `multipart_concurrency` and lower
  `multipart_part_size`.
- Keep an eye on `workers × multipart_concurrency` versus HAProxy `maxconn`
  and the New cluster's connection limits.

### 7.6 Running on hosts where `/tmp` is tmpfs

The small-object path spools through a temp file. If your `/tmp` is backed by
tmpfs (in-memory), a large backlog of mid-sized objects can pressure the
kernel. Set `spool_dir` (or the `SPOOL_DIR` env var) to a path on real disk.
The default — the directory containing `db_path` — is already a sane choice
on most systems.

---

## 8. Safety guarantees

| Guarantee                                       | How it is enforced                                                        |
| ----------------------------------------------- | ------------------------------------------------------------------------- |
| Never overwrite an object on New                | Mandatory `HeadObject(New)` precheck before any write                     |
| Never delete an object on either cluster        | The code contains **no** `DeleteObject` call anywhere                     |
| Never operate on the wrong bucket               | `HeadBucket` precheck + per-page `Name` validation + `meta` row binding `db_path` to the configured bucket |
| Survive source-side deletes during migration    | `NoSuchKey` / 404 on Old is treated as `skipped`, not as a failure        |
| Survive crashes and restarts                    | Status persisted per object in SQLite (WAL mode)                          |
| Bounded memory regardless of object size        | Small files spool to disk; multipart uses bounded parts                   |
| Verify data after copy                          | Size + ETag check for small files; size check for multipart               |
| No server-side copy                             | The code does not call `CopyObject`; everything is GET-then-PUT           |

---

## 9. Troubleshooting

| Symptom                                                            | Likely cause / fix                                                                                                              |
| ------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------- |
| `aws config: …` on startup                                         | Bad endpoint URL or unreachable host; verify with `curl -v <endpoint>`                                                          |
| Repeated `head new: … 403 AccessDenied`                            | The New credentials lack `s3:HeadObject`; grant read+write on the bucket                                                        |
| Many objects in `failed` with `etag mismatch`                      | Old has SSE / compression that mutates ETag; widen `multipart_threshold` to skip the comparison, or accept it after manual audit |
| `size mismatch after multipart`                                    | Another writer touched the same key on New during your upload; re-run `migrate`                                                 |
| SQLite `database is locked`                                        | Two `migrate` processes against the same `db_path`; run only one at a time                                                      |
| Process won't exit on `Ctrl-C`                                     | An `UploadPart` is still in flight — wait a few seconds, or press `Ctrl-C` twice                                                |
| `mkdir /…/rustfs-migration-*: no such file or directory` on PUT    | `spool_dir` does not exist; create it or change the setting                                                                     |
| Process exits non-zero with `FATAL … cancelling remaining work`    | A persistence error (SQLite or `conflict.log`) was surfaced; inspect local disk, fix the cause, then re-run `migrate` to resume |
| `db_path is bound to bucket "X" but config requests "Y"`           | `db_path` was previously used for bucket X; choose a different `db_path` for bucket Y (one DB per bucket)                       |
| `head bucket "…": …` immediately on `scan` / `migrate`             | The bucket does not exist on Old, or credentials/endpoint are wrong. The tool refuses to enumerate before this passes.          |
| `list page bucket mismatch: requested "X", server returned "Y"`    | The S3 endpoint silently routed the request to a different bucket (e.g. a misconfigured proxy). Fix routing; do **not** retry blindly. |

---

## 10. Exit codes

| Code | Meaning                                                  |
| ---- | -------------------------------------------------------- |
| 0    | Command completed successfully                           |
| 1    | Configuration, persistence, or remote error              |
| 130  | Second `SIGINT` / `SIGTERM` received during graceful stop |
