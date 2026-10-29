# migration — operator usage

Safe one-way copy between two S3-compatible clusters (RustFS, MinIO, S3). Never deletes. A target object that does not match the inventory is never overwritten. The Chinese manual is [使用手册.md](./使用手册.md). This file covers the same operational facts, including the shared byte-rate cap from the rate-limit work.

```bash
cp config.example.yaml config.yaml
./bin/migration run  -c config.yaml --workers 16 --rate-limit 20M
./bin/migration diff -c config.yaml
```

`run` exiting 0 means no fatal error. Per-object failures stay in SQLite as `failed` and do not fail the process. Check `status`, then `diff`.

## Build

Go 1.22 or newer. SQLite is pure Go. `CGO_ENABLED=0` produces a statically linked ELF.

```bash
make build                 # bin/migration for this machine
make build-linux-amd64     # bin/migration-linux-amd64   (Linux x86_64)
make build-linux-arm64     # bin/migration-linux-arm64   (Linux arm64)
make build-linux            # both
```

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/migration-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/migration-linux-arm64 .
```

`bin/` is gitignored. `file` should report `statically linked`, `x86-64` or `ARM aarch64`.

## Workers and rate_limit

Priority is **CLI > environment > YAML > default**.

| Knob | Flag | Env | YAML | Default | Applies to |
| --- | --- | --- | --- | --- | --- |
| Object workers | `--workers` | `WORKERS` | `workers` | 16 | `run` / `migrate` copy, and `diff` HEAD |
| Parts of one large object | `--multipart-concurrency` | `MULTIPART_CONCURRENCY` | `multipart_concurrency` | 4 | multipart upload inside `run` / `migrate` |
| Aggregate source read | `--rate-limit` | `RATE_LIMIT` | `rate_limit` | unlimited (`0` or omit) | object bodies in `run` / `migrate` |

`scan` is single-threaded. `--workers` does not speed it up, and `rate_limit` does not apply. `list` and `status` use neither knob. `diff` uses `workers` for HEAD and does not read object bodies, so the byte cap does not apply. `status` does not call S3.

For the concurrency knobs, YAML `0` or omit means the default. A negative value is an error. An explicit `0` on the CLI or in `WORKERS` / `MULTIPART_CONCURRENCY` is also an error. For `rate_limit`, `0` means unlimited at every layer, including `--rate-limit 0` and `RATE_LIMIT=0`.

Peak in-flight requests are about `workers × multipart_concurrency`. The default `16 × 4` can exhaust HAProxy `maxconn` even when `rate_limit` is set, because the cap counts bytes, not connections. Lower `workers` first when the proxy returns 503, then `multipart_concurrency` for large objects.

### Byte-rate units

`rate_limit` is one shared token bucket. Every copy worker, including parts of one large object, draws from it. Buckets are copied one at a time, so the number is the process-wide cap on source object bytes.

| Text | Bytes/s |
| --- | --- |
| `20M`, `20MB` | 20×1000×1000 = 20000000 |
| `30M` | 30000000 |
| `50M`, `50MB` | 50000000 |
| `20Mi`, `20MiB` | 20×1024×1024 = 20971520 |
| bare integer, e.g. `1048576` | that many bytes/s |
| `0`, `0M`, or omit | unlimited |

`K`/`KB` = 1000, `Ki`/`KiB` = 1024; `G`/`GB` = 1000³, `Gi`/`GiB` = 1024³. Suffixes are case-insensitive. An optional `/s` is ignored. `M` is not `Mi`.

The bucket charges source body reads (the small-object spool and each multipart range). List, HEAD, and the upload itself are not charged a second time. A small object is read at the cap, then uploaded; the target side can briefly run faster. If the migration host shares a NIC with production, leave headroom for that write.

Startup logs the value actually used. `20M` prints as `rate_limit=20MB/s`. `50M` prints as `rate_limit=50MB/s`. Unlimited prints as `unlimited`.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0
```

The first Ctrl-C cancels a read that is waiting on the bucket. Unfinished rows stay in SQLite. The cap does not overwrite or delete.

`multipart_part_size` must be at least 5 MiB (5242880). A smaller configured value fails at load and is not raised. `0` becomes the 16 MiB default first. S3 rejects a smaller non-final part with `EntityTooSmall`.

## Commands

Shared flags: `-c` / `--config` (default `config.yaml`), `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`. Only `diff` adds `--extra`.

| Command | Writes target | `workers` | `rate_limit` |
| --- | --- | --- | --- |
| `list` | no | no. One `ListBuckets`. `--bucket` does not filter | no |
| `scan` | no | no. One `ListObjectsV2` loop | no |
| `run` | yes, without overwriting conflicts | copy phase only. Scan stays single-threaded | copy phase only |
| `migrate` | yes. `pending` / `failed` only | yes | yes |
| `status` | no S3 | no | no |
| `diff` | no, and it does not create buckets | HEAD concurrency | no |

| Situation | Use |
| --- | --- |
| First copy, or the source has new keys | `run`, or `scan` then `migrate` |
| Count objects before any write | `scan` then `status`. `scan` does not need target credentials |
| After Ctrl-C or object errors, no new source keys | `migrate` only |
| A `skipped` key may have reappeared | `scan` again. `migrate` does not touch `skipped` |
| Acceptance | `diff`. It lists the source live and ignores the local inventory |

The scan completion line counts keys seen on this list, including keys already `done`. It is not a "new objects" count.

`diff` prints at most 50 issue lines, then `... and N more issues`. Summary counts are complete. Missing, size, or etag gaps exit 1. `--extra` does not. A missing target bucket makes every source key `missing`. The temp inventory is `migration-diff-<bucket>-<pid>.db` in the OS temp directory.

## State files

| Mode | How | State DB | Conflict log |
| --- | --- | --- | --- |
| All buckets | empty `bucket`, no `BUCKET`, no `--bucket` | `migration-<bucket>.db` next to `db_path` | `conflict-<bucket>.log` |
| One bucket | YAML bucket, `BUCKET`, or `--bucket` | `db_path` itself | `conflict_log` itself |

Do not mix them. A previous `--bucket photos` run writes `./migration.db`; an all-buckets run writes `migration-photos.db`. `status` can then show `photos` twice. With a bucket pinned, `status` opens `db_path` and creates it if missing (`TOTAL=0`). That empty file is not a completed scan.

One DB binds one bucket (`db_path is bound to bucket "X"`). A second process waits 5 seconds, then `database is locked`.

## Status meanings

1. HEAD of the target fails, and the failure is not a completed 404 (including cancel) → `failed`.
2. Target exists and matches the inventory → `done`, no upload.
3. Target exists and does not match → `conflict`, one line appended, no upload.
4. Target is absent and the source request finishes with 404 / `NoSuchKey` → `skipped`.
5. Cancel, deadline, `request canceled`, or `NoSuchUpload` from a multipart abort → `failed`, even when the text also contains `NotFound` or `StatusCode: 404`.
6. PUT or multipart succeeds and the follow-up HEAD matches → `done`.
7. Any other error → `failed`. The command can still exit 0.

| Status | `migrate` picks it up | Next `scan` |
| --- | --- | --- |
| `pending` | yes | refreshes size/etag |
| `failed` | yes | back to pending |
| `skipped` | no | back to pending if the key is listed again |
| `done` | no | left as-is |
| `conflict` | no | left as-is |

A match means the same size, and the same ETag when both are non-empty and neither contains `-`. A multipart ETag is not comparable. An identical pre-seeded object is `done`. A different size, or the same size with a different single-part ETag, is `conflict` and is never overwritten.

Conflict log (append-only TSV, UTC RFC3339):

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

There is no overwrite flag and no "retry conflicts" subcommand. Leave the row if the target copy should stay. To replace it, delete that key yourself, set the row back to `pending` or delete the row and `scan`, then `migrate`. While the differing object remains, the next pass is still a conflict.

Progress `copied` and `bytes` count rows marked `done` in this process, including objects that already matched and were not read. They are not NIC throughput and they are not the token-bucket charge. Historical totals are `status`.

## Ctrl-C and abort

The first `SIGINT` or `SIGTERM` cancels the pool, including a read waiting on `rate_limit`.

- Rows not started stay `pending`.
- The object in flight is marked `failed`, not `skipped`.
- An open multipart upload is aborted with `AbortMultipartUpload` on a fresh 30-second context. Abort drops the upload id only. There is no `DeleteObject`.
- A second signal calls `os.Exit(130)` and can cut the abort short.
- `kill -9` skips the abort. Leftover upload ids are not reaped; the next copy starts a new upload. Expire incomplete multiparts with bucket lifecycle.

If the only bucket is in the copy phase, the first Ctrl-C can exit 0. Cancel during scan, or between buckets, exits 1. Trust `status`.

A PUT that landed before `done` was recorded resumes as `done` when the target matches, and as `conflict` when it does not.

## Metadata kept, and what is not migrated

Both copy paths write `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, and `x-amz-meta-*`. Those HEAD/GET headers are not what the token bucket charges; the body bytes are.

Not migrated: versions and delete markers, Object Lock, tags, ACL, storage class, SSE-C / SSE-KMS parameters, bucket policy, lifecycle, CORS, replication, notifications, website, logging, default encryption, and server-side `CopyObject`.

If a single-part ETag check fails after the PUT, the row is `failed` and the bytes may already be on the target. The next `migrate` records `conflict` and does not overwrite.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | No error returned. `failed` rows can still exist. A first Ctrl-C during a single-bucket copy can also exit 0 |
| 1 | Bad config, remote error, a failed bucket, `diff` gaps, or cancel during scan / between buckets |
| 130 | Second `SIGINT` / `SIGTERM` |

Done means `PENDING=0`, `FAILED=0`, and `diff` exits 0.

## Troubleshooting

| Symptom | What to do |
| --- | --- |
| Cap production near 20 MB/s | `--rate-limit 20M` (20000000 B/s). Binary units are `20MiB`. Confirm the log says `rate_limit=20MB/s` |
| `50M` in YAML or on the CLI | 50000000 B/s. The log prints `rate_limit=50MB/s` |
| `unknown size suffix` or `rate limit` | Use `20M` for decimal or `20MiB` for 1024-based units |
| `workers must be >= 1` | YAML `0` means default 16. CLI and `WORKERS=0` are errors |
| `multipart_part_size must be >= 5242880` | Raise it. `0` becomes 16 MiB. Values from 1 to 5242879 fail |
| Source still busy after `20M` | The cap is source body bytes. Uploads are extra. Lower the number if the NIC is shared |
| Scan or diff did not slow down | Expected. The cap applies only while copying object bodies |
| `run` exits 0 but `diff` reports missing | Read `last_error`, then `migrate` |
| `etag mismatch` after copy | Often SSE. The next pass records `conflict` and will not overwrite |
| `status` empty after an all-buckets run | Look for `migration-<bucket>.db`. `--bucket` reads `db_path` |
| `x509` / `certificate` | No TLS-skip flag |
| Incomplete multipart after `kill -9` | Not reaped. Use bucket lifecycle |

## RustFS 1.0.4 checklist

1. `list` shows the source buckets.
2. `run --rate-limit 20M` or `50M` on a small bucket. The log shows `rate_limit=20MB/s` or `rate_limit=50MB/s`, not `unlimited`.
3. Ctrl-C once. Interrupted rows are `pending` or `failed`, not `skipped`. `migrate` until `FAILED=0`.
4. `run` again. `done` rows are not uploaded. `diff` exits 0.
5. Pre-seed a same-named object with a different size or a different single-part ETag. That key becomes `conflict`, the target bytes stay, and `conflict.log` gains one line.

Design notes: [DESIGN.md](./DESIGN.md).
