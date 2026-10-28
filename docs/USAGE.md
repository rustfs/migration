# migration — English usage (short)

The full operator manual is in Chinese: [使用手册.md](./使用手册.md).

```bash
cp config.example.yaml config.yaml
./bin/migration --help
./bin/migration run  -c config.yaml
./bin/migration diff -c config.yaml
```

- Empty `bucket:` migrates every source bucket.
- `--bucket NAME` pins one bucket.
- `run` = scan + copy. `scan` / `migrate` stay available separately.
- `diff` HEADs the target for every source key; exit 1 if anything is missing or mismatched.
- Never overwrites. Never deletes. Re-run after Ctrl-C.
- A target object that already matches the inventory (same size; same ETag when both are present and neither is a multipart ETag `hash-N`) is marked `done`, not `conflict`. A different size or comparable ETag is still a conflict and is never overwritten. A Ctrl-C after a successful PUT therefore resumes as `done` instead of a false conflict.
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

Safety, state machine, tuning and troubleshooting: see the Chinese manual and `docs/DESIGN.md`.
