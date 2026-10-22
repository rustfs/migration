# migration — English usage (short)

The operator manual is in Chinese: [使用手册.md](./使用手册.md).
The README has the same commands in both languages.

```bash
cp config.example.yaml config.yaml
./bin/migration --help
./bin/migration run  -c config.yaml --workers 16
./bin/migration diff -c config.yaml
```

- Empty `bucket:` migrates every source bucket. `--bucket NAME` pins one.
- `run` = scan + copy. `scan` / `migrate` stay available separately.
- `scan` is single-threaded. `--workers` does not speed it up.
- `--workers` (env `WORKERS`, default 16) is how many objects `run` / `migrate` copy at once, and how many keys `diff` HEADs at once.
- `--multipart-concurrency` (env `MULTIPART_CONCURRENCY`, default 4) is how many parts of **one** large object upload together.
- Priority: CLI > environment > YAML > default. `0` in YAML means "default". A negative value, or an explicit `0` on the CLI or in the environment, is an error.
- Peak HTTP per cluster is about `workers × multipart_concurrency`.
- `diff` HEADs the target for every source key and exits 1 when anything is missing or mismatched. It never uploads.
- Never overwrites. Never deletes. The first Ctrl-C cancels the pool; `migrate` resumes from SQLite. A second signal exits 130.
- A target object that already matches the inventory (same size; same ETag when both are present and neither is a multipart ETag `hash-N`) is marked `done`, not `conflict`. A different size or comparable ETag is still a conflict and is never overwritten. A Ctrl-C after a successful PUT therefore resumes as `done` instead of a false conflict.
- `skipped` is only a source 404 / NoSuchKey from a request that finished. `context.Canceled`, `context.DeadlineExceeded`, and `request canceled` are `failed` (the next `migrate` retries them), even when the error text also contains `NotFound` or `StatusCode: 404`.
- `multipart_part_size` must be at least 5 MiB (5242880). S3 rejects a smaller non-final part with `EntityTooSmall`. The 16 MiB default is unchanged. A smaller configured value fails at config load and is not clamped upward.

## Limiting load

There is no bandwidth or MB/s setting. `bandwidth` and `rate_limit` are not config fields, and there is no matching environment variable. Throughput is only concurrency in `config.yaml`:

| Field | Meaning | Default |
| --- | --- | --- |
| `workers` | Objects copied at once. Also the HEAD concurrency for `diff`. | 16 |
| `multipart_concurrency` | Parallel parts of one large object (`run` / `migrate` only). | 4 |

Peak concurrency is about `workers × multipart_concurrency`. Small objects use one request each; objects at or above `multipart_threshold` multiply by the part concurrency. The default `16 × 4` can saturate the NIC, either cluster, and HAProxy `maxconn`.

To aim for lower network or API load (for example around 20 MB/s), lower `workers` first. If large objects still fill the link, lower `multipart_concurrency` as well. Watch `migration status` for local progress and watch the NIC and cluster load on the host (`status` does not call S3). Actual MB/s depends on object size and the path; the config cannot express a target rate.

```yaml
# Starting point for a lighter run. Adjust after watching status and the NIC.
workers: 2
multipart_concurrency: 2
```

To go faster when the link has headroom: raise `workers` for many small objects, or raise `multipart_concurrency` / lower `multipart_part_size` (bytes, default 16 MiB, minimum 5 MiB; a smaller value fails at load and is not raised) for a single large object.

Safety, the state machine, tuning, and troubleshooting: see the Chinese manual and `docs/DESIGN.md`.
