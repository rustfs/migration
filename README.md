# migration

`migration` copies objects one way between two S3-compatible clusters. It targets self-hosted RustFS and MinIO, and any other endpoint that speaks the S3 API. Requests always use path-style addressing, which is the usual setup for those clusters.

Run it next to live traffic. Three rules hold for every command that writes:

- **Never overwrites.** Before any write, the tool calls `HeadObject` on the target. A match is recorded as finished. A different object is logged and left untouched.
- **Never deletes.** The tool has no `DeleteObject` call. Aborting a multipart upload drops only that unfinished upload, not an object that was already committed.
- **Resumable.** Every object has a row in a local SQLite database. Stop the process and start it again; finished work is not repeated.

A common layout is an older cluster that still holds history, a newer cluster that receives all application writes, and an application that already reads from both. This tool backfills keys the target does not yet have. It will not replace a newer object on the target with older bytes from the source. Same-key conflicts are expected in that layout. Read `conflict.log` and resolve them by hand.

The copy carries the current object, plus `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, and `x-amz-meta-*` user metadata. Version history, delete markers, Object Lock, bucket policies, and lifecycle rules stay where they are.

In the config file the source cluster is `old` and the target cluster is `new`.

中文操作手册：[docs/使用手册.md](docs/使用手册.md)。本文件是英文使用说明。

## Documentation

| Guide | What it is |
| --- | --- |
| This README | English install, configuration, commands, tuning, and safety |
| [docs/USAGE.md](docs/USAGE.md) | Short English companion for the same behavior |
| [docs/使用手册.md](docs/使用手册.md) | Full Chinese handbook |
| [docs/DESIGN.md](docs/DESIGN.md) | Architecture and the constraints the tool will not relax |

`migration --help` and `migration <command> --help` print bilingual English and Chinese help.

## Quick start

Go 1.22 or newer. SQLite is pure Go, so the binary does not need CGO.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # source and target endpoints and keys
CGO_ENABLED=0 go build -o bin/migration .
# or: make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Leave `bucket:` empty to migrate every bucket on the source. Set `bucket: photos`, or pass `--bucket photos`, to pin one bucket.

`20M` is a decimal megabyte per second: 20×1000×1000 bytes/s. See [Rate limit](#rate-limit) before raising it on a shared network.

## Install

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` runs the same command. The result is a static binary at `bin/migration`. `make test` runs `go test ./...`.

Protect `config.yaml` (`chmod 600` is a reasonable default). The process logs endpoints and object keys. It does not log credentials.

## Configuration

```bash
cp config.example.yaml config.yaml
```

The minimum is an endpoint, access key, and secret key for each side. The target key needs `s3:CreateBucket` when the bucket is not already there. An existing target bucket is left as it is.

```yaml
old:                              # source, read-only
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # target, read and write
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # empty = every bucket on the source
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # small-object temp dir; default is the db directory

workers: 16                       # objects at once; 0 or omit = 16
multipart_threshold: 52428800     # >= 50 MiB uses multipart upload
multipart_part_size: 16777216     # 16 MiB; must be >= 5 MiB (5242880)
multipart_concurrency: 4          # parts of one large object; 0 or omit = 4
rate_limit: 0                     # aggregate source-read cap; 0 or omit = unlimited
```

`list` and `scan` need the source endpoint and keys. `run`, `migrate`, and `diff` need both sides.

### Precedence

Highest priority wins:

1. CLI: `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`
2. Environment variables
3. YAML
4. Built-in defaults

| Setting | Default when omitted |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB (52428800) |
| `multipart_part_size` | 16 MiB (16777216) |
| `rate_limit` | unlimited |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | directory of `db_path` |
| `region` | `us-east-1` |

Example: YAML `workers: 8`, environment `WORKERS=32`, and `--workers 4` together use **4**.

### What zero means

`0` is not the same knob on every field.

| Field | YAML `0` or omitted | Environment `0` | CLI `0` |
| --- | --- | --- | --- |
| `workers` | default 16 | error | error (`--workers 0`) |
| `multipart_concurrency` | default 4 | error | error |
| `multipart_threshold` | default 50 MiB | no env var | no flag |
| `multipart_part_size` | default 16 MiB | no env var | no flag |
| `rate_limit` | unlimited | unlimited (`RATE_LIMIT=0`) | unlimited (`--rate-limit 0`) |

A negative concurrency or size fails at startup. `WORKERS` and `MULTIPART_CONCURRENCY` must be plain integers. Put size suffixes on `RATE_LIMIT` only: `RATE_LIMIT=20M` is valid, `WORKERS=20M` is not.

`multipart_part_size` must be at least 5 MiB (5242880). S3 rejects a smaller non-final part with `EntityTooSmall`. A smaller value fails when the config is loaded. It is not raised for you. If one object would need more than 10,000 parts, the tool grows that object's part size so the upload fits.

A bucket name must not contain whitespace or `/`.

### Environment variables

Set variables override the YAML values. Unset variables leave the file alone.

| Variable | Overrides |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | source endpoint |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | target endpoint |
| `REGION` | either side whose region is still empty after the specific variables |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | local paths |
| `WORKERS` `MULTIPART_CONCURRENCY` | concurrency (integer ≥ 1) |
| `RATE_LIMIT` | aggregate source-read cap (`20M`, `20MiB`, `0`, …) |

## Commands

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| Command | What it does | Writes to the target | Concurrency and cap |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | List every bucket on the source | No | None |
| `migration run -c config.yaml` | **Recommended.** Scan, then copy | Yes, without overwriting | Scan is single-threaded. Copy uses `workers`, `multipart_concurrency`, and `rate_limit` |
| `migration scan -c config.yaml` | Inventory the source into SQLite | No | Single-threaded. No rate limit |
| `migration migrate -c config.yaml` | Copy SQLite rows in `pending` or `failed` | Yes, without overwriting | `workers`, `multipart_concurrency`, and `rate_limit` |
| `migration status -c config.yaml` | Print local counters | No | None. No S3 calls |
| `migration diff -c config.yaml` | List the source and `HEAD` each key on the target | No | `workers` for parallel HEAD. No rate limit |
| `migration diff --extra -c config.yaml` | Same, and list keys that exist only on the target | No | `workers` |

Flags shared by every command:

| Flag | Meaning |
| --- | --- |
| `-c`, `--config` | Config file. Default `config.yaml` |
| `--bucket NAME` | Pin one bucket. Overrides YAML and `BUCKET` |
| `--workers N` | Objects copied at once for `run` / `migrate`, and parallel HEAD for `diff`. Must be ≥ 1 |
| `--multipart-concurrency N` | Parts uploaded at once inside one large object. Must be ≥ 1. Ignored by `list`, `scan`, `status`, and `diff` |
| `--rate-limit SIZE` | Aggregate source-read cap for object bodies in `run` / `migrate`. `0` is unlimited |
| `--version` | Print the version |
| `--help` | Bilingual help |

`diff` also accepts `--extra`.

`status` prints `BUCKET`, `TOTAL`, `PENDING`, `DONE`, `CONFLICT`, `SKIPPED`, and `FAILED`. With no bucket pinned, it reads `migration-*.db` next to `db_path`, and the configured database itself when that file is already bound to a bucket. If nothing is there yet, it says so and exits 0. Run `scan` or `run` first.

## Workflows

### Migrate every bucket

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

For each bucket, `run` does the following:

1. `HeadBucket` on the source.
2. Create the bucket on the target when it is missing. An existing bucket is not modified.
3. `ListObjectsV2` the source into SQLite. This step is single-threaded.
4. Start `workers` copy goroutines. Each object is checked with `HeadObject` on the target first. A missing key is read from the source and written to the target. Those source reads share `rate_limit`. A key that already matches the inventory is marked `done` and is not uploaded. A key that exists and does not match is appended to the conflict log and is not overwritten.
5. An object at or above `multipart_threshold` uploads its parts with `multipart_concurrency`. Those parts share the same byte cap.
6. Check the size on the target. A single-part object is also checked by ETag when the ETags are comparable. See [Object state](#resume-and-object-state).

Buckets are copied one after another, so `rate_limit` is the cap for the whole process.

### One bucket

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### Objects added on the source later

`ListObjectsV2` cannot filter by modification time on the server. Catch-up is another full listing. New keys become `pending`. Rows that were `failed` or `skipped` return to `pending` when they are listed again. Rows that are `done` or `conflict` stay as they are, so a second pass does not re-upload them and does not overwrite the target.

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

`run` again is the same two steps in one command.

### Inventory first

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # object counts, no S3 calls
./bin/migration migrate -c config.yaml --workers 16
```

## Concurrency

Three controls, and they do different jobs:

| Control | What it limits | Raise it when | Lower it when |
| --- | --- | --- | --- |
| `workers` / `--workers` | How many **objects** are in flight. Also the HEAD parallelism for `diff` | Mostly small objects, and CPU, disk, and the proxy still have headroom | 503s, timeouts, a saturated source disk, or you are debugging one object (`--workers 1`) |
| `multipart_concurrency` / `--multipart-concurrency` | How many **parts of one large object** upload together | A single object is large and that flow can use more of the link | Large objects already fill the link, or several of them together fill the proxy |
| `rate_limit` / `--rate-limit` | How many **source bytes per second** all of those workers may read, combined | The source and the network still have headroom | Production is using the source cluster or the same NIC. Start at `20M`, then try `30M` or `50M` |

`scan` ignores `workers` and `rate_limit`. Listing is one page after another because each page needs the previous continuation token, and the rows land in one SQLite file. A slow scan is not fixed by `--workers`. `list`, `diff`, and `status` do not read object bodies, so the byte cap does not apply to them. `diff` does use `--workers` for HEAD.

```text
workers = 4 objects at once
  object A (small)  ── one GET + PUT
  object B (small)  ── one GET + PUT
  object C (large)  ── multipart_concurrency ranged GETs + UploadPart
  object D (large)  ── the same
```

Peak in-flight requests against one cluster are about `workers × multipart_concurrency`. A small object uses one request. An object at the multipart threshold multiplies by the part concurrency. The default `16 × 4` is 64. HAProxy or Nginx `maxconn`, and the disks on both clusters, have to carry that product.

The HTTP client raises idle connections per host to follow that product (capped at 1024), instead of Go's default of two idle connections per host. That is connection reuse. The worker pool and the part semaphore are what bound in-flight work. The waiting queue holds at most 2048 object descriptors, so a large `--workers` does not load the whole bucket into memory.

Starting points:

| Workload | Suggestion |
| --- | --- |
| Many small objects (kilobytes to a few megabytes), link has room | `workers` 16–64, leave `multipart_concurrency` at 4 |
| Mixed sizes | `workers` 8–32, `multipart_concurrency` 4–8 |
| A few very large objects | `workers` 2–8, `multipart_concurrency` 4–16 |
| One object failing and the log is hard to read | `--workers 1` |
| Proxy returns 503, timeouts, or the source disk is full | Lower `workers` first. If the slow objects are large, lower `multipart_concurrency` as well |
| Keep production bandwidth | `--rate-limit 20M`, then `30M` or `50M` after the source looks healthy. `0` removes the cap |
| Faster small objects | Raise `workers` |
| Faster one large object | Raise `multipart_concurrency`, or lower `multipart_part_size` (bytes, minimum 5 MiB) |

During a copy, a progress line is logged about every 10 seconds: processed, copied, conflicts, skipped, failed, and bytes.

Each small object in flight has one temp file in `spool_dir`, up to about `multipart_threshold` bytes, because `PutObject` needs a seekable body. If `/tmp` is a memory disk, point `spool_dir` at a real disk. More workers means more of those files at once.

Use one process per database file. SQLite is opened in WAL mode with a single connection inside the process, so status updates stay serialized while object bytes stay on the worker pool. A second process on the same file still reports `database is locked`.

## Rate limit

`rate_limit` is one shared token bucket for every copy worker, including the parts of a large object. It counts bytes read from source object bodies. List, HEAD, and the pre-write conflict check are outside the cap. The upload that follows a completed read is outside the cap as well.

A small object is read from the source at the cap, then written to the target. Source reads stay within the cap. The following write can be briefly faster. If the migration host shares a NIC with production, that write still uses bandwidth on top of the capped read. When total NIC use should stay near 40 MB/s, start at `20M`.

The cap slows copies. It does not overwrite, and it does not delete. An object that already matches the inventory is still skipped. The first Ctrl-C cancels a read that is waiting for tokens. The unfinished object stays in SQLite for the next `migrate`.

The cap does not reduce how many requests are in flight. A proxy that returns 503 is still a concurrency problem: lower `workers`, then `multipart_concurrency`.

### Units

Suffixes are case-insensitive. A trailing `/s` is accepted and ignored. A bare number is bytes per second. `1.5M` is 1,500,000 bytes/s.

| Written as | Bytes per second |
| --- | --- |
| `20M`, `20MB` | 20×1000×1000 = 20000000 |
| `30M` | 30000000 |
| `50M`, `50MB` | 50000000 |
| `20Mi`, `20MiB` | 20×1024×1024 = 20971520 |
| `2K`, `2KB` | 2000 |
| `2Ki`, `2KiB` | 2048 |
| `1G`, `1GB` | 1000000000 |
| `1Gi`, `1GiB` | 1073741824 |
| `1T`, `1TB` / `1Ti`, `1TiB` | 1000⁴ / 1024⁴ |
| `1048576` | 1048576 |
| `0`, `0M`, or omit | unlimited |

`M` is decimal. `Mi` is binary. `20M` is about 5% under `20MiB` (20000000 versus 20971520). A negative value or an unknown suffix fails at startup.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # unlimited, on purpose
```

## Resume and object state

The first Ctrl-C or `SIGTERM` cancels the worker pool. Objects that have not started stay `pending`. An in-flight multipart upload is aborted, which releases that upload and does not delete a committed object. State already written to SQLite is kept. A second signal exits immediately with code 130.

If the target `PutObject` succeeded and the row was not yet marked `done`, the row stays `failed` (or `pending`) even though the object is on the target. The next `migrate` checks the target with `HeadObject`. When the size matches and any comparable ETag matches, the row is marked `done`. When the size differs, or a comparable ETag differs, the row is marked `conflict`, and the object is still not overwritten.

A canceled request, a deadline, and `request canceled` are `failed`, including when the error text also contains `NotFound` or `StatusCode: 404`. A multipart abort often reports `NoSuchUpload`. That is not a missing source object. `skipped` means the source request finished and the object was really gone.

```bash
./bin/migration migrate -c config.yaml
```

`migrate` retries `pending` and `failed` only.

| State | Meaning | A later scan |
| --- | --- | --- |
| `pending` | Waiting to copy | Refreshes size and ETag |
| `failed` | Last attempt failed. `migrate` retries it. `last_error` has the message | Returns the row to `pending` |
| `skipped` | The source request finished with 404 / `NoSuchKey` | Returns the row to `pending` if the key is listed again |
| `done` | Copied and checked, or the target already matched the inventory | Left unchanged |
| `conflict` | The target has the key and it does not match the inventory. Not overwritten | Left unchanged |

**Match rule.** Size must equal the inventoried size. ETags are compared only when both sides have one and neither is a multipart ETag (a value containing `-`, shaped like `hash-N`). Multipart ETags depend on part boundaries, so they are not compared; size is the check. A different size, or a comparable ETag that differs, is a conflict.

Each conflict is one TSV line in the conflict log, with RFC3339 UTC timestamps:

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

One configured bucket uses `db_path` and `conflict_log` as written. Every-bucket mode derives sibling files:

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

One database file is bound to one bucket. Pointing it at another bucket fails with `db_path is bound to bucket "…"`.

## Checking the target with `diff`

`diff` lists the source bucket live and `HEAD`s each key on the target. It does not read the migration SQLite inventory, and it does not upload.

```bash
./bin/migration diff -c config.yaml --workers 32
./bin/migration diff -c config.yaml --extra
```

```text
=== diff photos ===
ok=1000  missing=2  size_mismatch=0  etag_mismatch=1  extra_on_target=0
一致=1000  缺失=2  大小不一致=0  ETag不一致=1  仅目标有=0
  missing  a.txt  not on target
  etag     b.txt  src=abc dst=def
```

The second summary line repeats the counters in Chinese, because that is what the command prints. At most 50 issue lines are shown; the rest are counted in a trailing “and N more” line.

| Result | Meaning |
| --- | --- |
| `missing` | On the source, not on the target. Run `migrate` or `run` again |
| `size` / `etag` | Same key, contents do not match. Often a live write on the target. This tool will not overwrite it |
| `extra` | Only on the target. Reported with `--extra`. Usually a write that already landed on the new cluster. Not a failure |

Exit code is 1 when any key is missing or mismatched in size or ETag. `extra_on_target` does not fail the command. With several buckets, a gap fails that bucket and the command continues with the rest, then exits 1 if any bucket failed.

## Safety

| Guarantee | How |
| --- | --- |
| No overwrite of a target object | `HeadObject` before every write. A match is `done`. A mismatch is `conflict.log` and a skip |
| No delete | No `DeleteObject`. A failed multipart upload is aborted only |
| No cross-bucket mix-up | `HeadBucket`, a bucket-name check on each list page, and a bucket name stored in the SQLite file |
| Source deletes during the run | A finished source 404 is `skipped`. Cancel and timeout are `failed` and will be retried |
| Crash and cancel resume | Per-object state is in a SQLite WAL file. The first signal cancels workers; the next `migrate` continues |
| Bounded memory | Small objects spool to disk. Large objects are read by part. The job queue is capped |
| Check after copy | Size always. ETag as well for a single-part object when both ETags are comparable |

Out of scope: version history, delete markers, Object Lock, bucket policies, lifecycle rules, two-way sync, and server-side `CopyObject` (the clusters are separate, so the bytes go through this process).

## Troubleshooting

| What you see | What to do |
| --- | --- |
| `read config … copy config.example.yaml first` | The `-c` path is missing. Copy `config.example.yaml` |
| `old.endpoint is required` | Set `old.endpoint` or `OLD_ENDPOINT`. The target side reports `new.endpoint is required` the same way. Keys report `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`, `WORKERS`, or YAML `workers` is below 1. YAML `0` means “default 16”. `0` on the CLI or in the environment is an error |
| `multipart_concurrency must be >= 1` | Same rule, for part concurrency. YAML `0` means “default 4” |
| `env WORKERS="…" is not an integer` | `WORKERS` must be a bare integer. Bandwidth belongs in `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | Non-final parts must be at least 5 MiB. The default 16 MiB is already valid. The value is not raised automatically |
| `unknown size suffix` or `rate limit` | `rate_limit`, `RATE_LIMIT`, or `--rate-limit` was not recognized. Decimal is `20M`. Binary is `20Mi` or `20MiB` |
| `head bucket "…"` | The bucket is missing, the endpoint is wrong, or the key cannot read it. On the target, a create failure says `create bucket "…" on target (need s3:CreateBucket)` |
| Source still busy at `20M` | `20M` is 20000000 bytes/s of source body reads. The upload is outside that cap. On a shared NIC, lower the number |
| `scan` or `diff` did not slow down | Expected. The cap applies while `run` / `migrate` read object bodies |
| Scan stays slow after raising `--workers` | Expected. `scan` is single-threaded. Concurrency starts at the copy, and at `diff` HEAD |
| Many 503s or timeouts | Lower `workers`. For large objects, lower `multipart_concurrency` so `workers × multipart_concurrency` fits the proxy `maxconn` |
| Many `etag mismatch` results | Source SSE or compression can change the ETag. Lower `multipart_threshold` to take the multipart path (size is checked, ETag is not compared), or sample the objects by hand |
| `db_path is bound to bucket "…"` | That file already belongs to another bucket. Use a different `db_path` |
| `database is locked` | Two processes are writing the same database. Use one process per file |
| Disk fills with temp files | Move `spool_dir` to a large disk, or lower `workers` |
| Ctrl-C does not return | Wait for the current request, or press it again (exit 130) |
| `diff` still reports `missing` | Run `migrate` again and read `last_error` on `failed` rows |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. For a single bucket, the first Ctrl-C during the copy phase (`migrate`, or the copy half of `run`) also exits 0. Unfinished objects remain in SQLite |
| 1 | Bad config, a remote error, a bucket failed, or `diff` found a missing or mismatched key. Also the first Ctrl-C during `scan`, and an interrupt while more than one bucket is in the run |
| 130 | A second `SIGINT` or `SIGTERM`. The process exits immediately |

Errors from the command are printed as `error: …` on stderr.

## License

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
