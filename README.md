# rf — RustFS bucket migration tool

A command-line tool that migrates buckets between two S3-compatible
(RustFS) clusters. Rust rewrite of
[github.com/rustfs/migration](https://github.com/rustfs/migration) (Go),
preserving its safety model: one-way, non-destructive, resumable.

In one sentence: **for every object that exists on the source but not on the
target, copy it to the target. Never overwrite. Never delete. Resume cleanly
after any interruption.**

## Features

- `rf list` — list every bucket on the source instance directly from your
  config.
- `rf migrate` — migrate the buckets **one by one** from source to target
  with a live progress display (per-bucket bars, object/byte counters).
- `rf status` — show per-bucket progress counters from the local state DBs.
- Safe by construction: mandatory `HeadObject` precheck on the target before
  any write, so live data on the target is **never overwritten**; conflicting
  keys are recorded in an append-only TSV log instead.
- Resumable: every object's state is persisted in SQLite (WAL mode). Re-run
  the same command after a crash or Ctrl-C and it continues where it left
  off.
- Automatic multipart upload for large objects (bounded memory via temp-file
  spooling for small objects and fixed-size parts for large ones).
- Object metadata preserved: `Content-Type`, `Cache-Control`,
  `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`
  and `x-amz-meta-*` user metadata.
- Copy verification: size check always, ETag check for single-part uploads.

## Build

Requires a Rust toolchain (1.80+). `cargo build --release` produces a static
binary at `target/release/rf`.

```bash
cargo build --release
# optional: install into ~/.cargo/bin
cargo install --path .
```

## Configuration

Copy the template and edit it:

```bash
cp config.example.yaml config.yaml
```

```yaml
old:
  endpoint:   "http://10.0.0.1:9000"     # source RustFS
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region:     "us-east-1"

new:
  endpoint:   "http://haproxy.new.local:9000"  # target RustFS
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region:     "us-east-1"

bucket: ""            # empty = migrate every bucket; or pin one bucket
db_path:      "./migration.db"
conflict_log: "./conflict.log"
spool_dir:    ""      # small-object temp dir; default = dir(db_path)

workers:               16
multipart_threshold:   52428800   # 50 MiB
multipart_part_size:   16777216   # 16 MiB
multipart_concurrency: 4
```

Path-style addressing is always forced (self-hosted RustFS / MinIO style),
and one client per endpoint is shared across all workers.

### Field reference

| Key                            | Required | Default          | Meaning                                                                 |
| ------------------------------ | -------- | ---------------- | ----------------------------------------------------------------------- |
| `old.endpoint`                 | yes¹     | —                | Source cluster URL                                                       |
| `old.access_key/secret_key`    | yes¹     | —                | Read credentials for the source                                          |
| `new.endpoint`                 | yes²     | —                | Target cluster URL                                                       |
| `new.access_key/secret_key`    | yes²     | —                | Read+write credentials for the target (needs `s3:CreateBucket` too)      |
| `bucket`                       | no       | *(all buckets)*  | Pin migration to a single bucket                                         |
| `db_path`                      | no       | `./migration.db` | SQLite state DB (single-bucket mode). See state layout below.            |
| `conflict_log`                 | no       | `./conflict.log` | Append-only TSV log of keys that already exist on the target             |
| `spool_dir`                    | no       | `dir(db_path)`   | Temp dir for small-object spooling (point at real disk if /tmp is tmpfs) |
| `workers`                      | no       | `16`             | Objects copied concurrently                                              |
| `multipart_threshold`          | no       | `50 MiB`         | Objects ≥ this size use multipart upload                                 |
| `multipart_part_size`          | no       | `16 MiB`         | Part size (auto-grown if an object would exceed 10 000 parts)            |
| `multipart_concurrency`        | no       | `4`              | Parts uploaded in parallel within one object                             |

¹ required for `rf list` / `rf migrate` · ² required for `rf migrate`.

### Environment variables

Environment variables override the YAML when set: `OLD_ENDPOINT`, `OLD_AK`,
`OLD_SK`, `OLD_REGION`, `NEW_ENDPOINT`, `NEW_AK`, `NEW_SK`, `NEW_REGION`,
shared fallback `REGION`, plus `BUCKET`, `DB_PATH`, `CONFLICT_LOG`,
`SPOOL_DIR`.

## Usage

```bash
# 1. What buckets does the source have?
rf list -c config.yaml
# BUCKET                                       CREATED (UTC)
# photos                                       2026-01-01T00:00:00Z
# docs                                         2026-02-14T08:30:00Z
#
# 2 bucket(s) on http://10.0.0.1:9000

# 2. Migrate every bucket, one by one, with live progress
rf migrate -c config.yaml

#    or just one bucket
rf migrate --bucket photos -c config.yaml

# 3. Check counters any time
rf status -c config.yaml
```

`rf migrate` walks the buckets sequentially. For each bucket it:

1. `HeadBucket` on the source (fails fast on typos / permission problems) and
   creates the bucket on the target **only if it does not exist yet**.
2. **Scans** the source with `ListObjectsV2` pagination, buffering 500
   objects per transaction into the state DB (status `pending`).
3. **Copies** every `pending`/`failed` object through a worker pool:
   - `HeadObject` on the target first — if the key already exists there, the
     key is appended to the conflict log and marked `conflict`; the object on
     the target is **never touched**.
   - Small objects stream through a temp file in `spool_dir` and are
     `PutObject`-ed; large objects use
     `CreateMultipartUpload` → ranged `GetObject` + `UploadPart` (bounded
     concurrency) → `CompleteMultipartUpload`. Any failure aborts the
     multipart session.
   - After each copy: size verification, plus ETag comparison when both
     sides report a simple (non-multipart) ETag.
   - If the source returns 404 mid-run (the object was deleted after the
     scan) the object is marked `skipped`, not failed.
4. Prints a per-bucket summary line and moves on to the next bucket.

At the end a summary table is printed:

```
=== migration summary ===
BUCKET                           SCANNED    COPIED  CONFLICT   SKIPPED    FAILED  TRANSFERRED
docs                                   1         1         0         0         0        700 B
photos                                 3         2         1         0         0      5.0 MiB
TOTAL                                  4         3         1         0         0      5.0 MiB
```

### State layout (per bucket)

The Go original binds one `db_path` to one bucket. `rf` keeps that behaviour
in single-bucket mode and derives per-bucket files in all-buckets mode:

| Mode                   | State DB                        | Conflict log                                  |
| ---------------------- | ------------------------------- | --------------------------------------------- |
| `bucket:` set          | `db_path` verbatim              | `conflict_log` verbatim                       |
| `bucket:` empty (all)  | `<dir(db_path)>/rf-<bucket>.db` | `<dir(conflict_log)>/rf-<bucket>.conflict.log` |

The schema is identical to the Go tool's, so state DBs are interchangeable.

### Signals and exit codes

- First `SIGINT`/`SIGTERM`: graceful stop — in-flight objects finish or their
  multipart sessions are aborted, SQLite is flushed, exit code `130`.
- Second signal: force exit.
- Exit `0` success · `1` configuration / persistence / failed bucket.

Re-running `rf migrate` retries every `pending` or `failed` row — objects
already `done`/`conflict`/`skipped` are never re-uploaded. Re-scanning only
refreshes rows that are still `pending`/`failed`, so you can pick up objects
written to the source after the first scan by simply re-running `migrate`.

## Safety guarantees

| Guarantee                              | Enforced by                                                        |
| -------------------------------------- | ------------------------------------------------------------------ |
| Never overwrite an object on the target | Mandatory `HeadObject` precheck; conflicts logged, never written  |
| Never delete anything                  | No `DeleteObject` anywhere; multipart abort only releases sessions |
| Never operate on the wrong bucket      | `HeadBucket` precheck + per-list-page bucket-name validation       |
| Survive source-side deletes            | 404 during copy → `skipped`                                        |
| Survive crashes / restarts             | Per-object status in SQLite (WAL)                                  |
| Bounded memory                         | Small objects spool to temp files; multipart uses fixed-size parts |
| Verify after copy                      | Size + ETag (single-part) / size (multipart)                       |

## Differences from the Go original

- Whole-instance mode: the Go tool migrates exactly one configured bucket;
  `rf migrate` with an empty `bucket:` migrates **all** source buckets one by
  one (creating missing buckets on the target).
- Real-time progress bars (indicatif) instead of periodic log lines.
- Per-bucket conflict logs / state DBs in all-buckets mode (naming above).
- `scan` is folded into `migrate` (still resumable and idempotent);
  `rustfs-migration status` is available as `rf status`.

Everything else — the HEAD-precheck safety model, spool/multipart copy
paths, metadata preservation, verification, conflict log format, SQLite
schema, tunables and env-var overrides — mirrors the Go implementation.

## Troubleshooting

| Symptom                                       | Fix                                                                 |
| --------------------------------------------- | ------------------------------------------------------------------- |
| `head bucket "…"` fails at startup            | Bucket missing on source, wrong endpoint, or bad credentials        |
| `create bucket … on target` fails             | Target credentials lack `s3:CreateBucket`                            |
| Many `failed` rows with `etag mismatch`       | Source mutates ETags (SSE/compression); widen `multipart_threshold` |
| `db_path is bound to bucket "X"`              | One state DB per bucket — use a different `db_path`                  |
| Process won't exit on Ctrl-C                  | A part upload is in flight; wait a few seconds or Ctrl-C again       |
