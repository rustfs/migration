# migration — English usage (short)

The operator manual is in Chinese: [使用手册.md](./使用手册.md).
The README has the same commands in both languages.

```bash
cp config.example.yaml config.yaml
CGO_ENABLED=0 go build -o bin/migration .
./bin/migration --help
./bin/migration run  -c config.yaml --workers 16 --rate-limit 20M
./bin/migration diff -c config.yaml
```

Cross-compile a static Linux binary (no cgo). `linux/arm64` is the usual target for an arm64 server:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/migration-linux-arm64 .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/migration-linux-amd64 .
```

- Empty `bucket:` migrates every source bucket. `--bucket NAME` pins one.
- `run` = scan + copy. `scan` / `migrate` stay available separately.
- `scan` is single-threaded. `--workers` does not speed it up.
- `--workers` (env `WORKERS`, default 16) is how many objects `run` / `migrate` copy at once, and how many keys `diff` HEADs at once.
- `--multipart-concurrency` (env `MULTIPART_CONCURRENCY`, default 4) is how many parts of **one** large object upload together.
- `--rate-limit` (env `RATE_LIMIT`, YAML `rate_limit`, default unlimited) is the aggregate bytes per second those workers may read from the source. `20M` / `20MB` = 20×1000×1000 B/s. `20Mi` / `20MiB` = 20×1024×1024 B/s. `0` or omit means unlimited at every layer.
- Priority: CLI > environment > YAML > default. For the concurrency knobs, `0` in YAML means "default". A negative value, or an explicit `0` on the CLI or in the environment, is an error. For `rate_limit`, `0` is unlimited everywhere.
- Peak HTTP per cluster is about `workers × multipart_concurrency`.
- `diff` HEADs the target for every source key and exits 1 when anything is missing or mismatched. It never uploads.
- Never overwrites. Never deletes. The first Ctrl-C cancels the pool; `migrate` resumes from SQLite. A second signal exits 130.
- A target object that already matches the inventory (same size; same ETag when both are present and neither is a multipart ETag `hash-N`) is marked `done`, not `conflict`. A different size or comparable ETag is still a conflict and is never overwritten. A Ctrl-C after a successful PUT therefore resumes as `done` instead of a false conflict.
- `skipped` is only a source 404 / NoSuchKey from a request that finished. `context.Canceled`, `context.DeadlineExceeded`, and `request canceled` are `failed` (the next `migrate` retries them), even when the error text also contains `NotFound` or `StatusCode: 404`.
- `multipart_part_size` must be at least 5 MiB (5242880). S3 rejects a smaller non-final part with `EntityTooSmall`. The 16 MiB default is unchanged. A smaller configured value fails at config load and is not clamped upward.

## Limiting load

`--rate-limit` (env `RATE_LIMIT`, YAML `rate_limit`) caps aggregate source-read bytes per second across every copy worker. `20M` is 20×1000×1000 B/s. `0` or omit is unlimited. Request fan-out is still concurrency:

| Field | Meaning | Default |
| --- | --- | --- |
| `workers` | Objects copied at once. Also the HEAD concurrency for `diff`. | 16 |
| `multipart_concurrency` | Parallel parts of one large object (`run` / `migrate` only). | 4 |
| `rate_limit` | Shared source-read cap for `run` / `migrate` object bodies. | unlimited |

Peak in-flight requests are about `workers × multipart_concurrency`. The default `16 × 4` can still exhaust HAProxy `maxconn` even when `rate_limit` is set, because the cap counts bytes, not connections.

To protect production, start with `--rate-limit 20M` (or `30M` / `50M`). If the proxy returns 503, lower `workers` first, then `multipart_concurrency` for large objects. Watch `migration status` and the NIC (`status` does not call S3).

```yaml
# Starting point for a lighter run. Adjust after watching status and the NIC.
workers: 2
multipart_concurrency: 2
```

To go faster when the link has headroom: raise `workers` for many small objects, or raise `multipart_concurrency` / lower `multipart_part_size` (bytes, default 16 MiB, minimum 5 MiB; a smaller value fails at load and is not raised) for a single large object.

Safety, the state machine, tuning, and troubleshooting: see the Chinese manual and `docs/DESIGN.md`.
