# migration — English usage notes

The English operator guide is the [README](../README.md): install, configuration, workflows, tuning, safety, and exit codes. The same guide is in the repository as [简体中文](../README_ZH.md), [Deutsch](../README_DE.md), [français](../README_FR.md), and [日本語](../README_JA.md). The language bar on each README also links Español, 한국어, Portuguese, and Русский through readme-i18n.

This page is the short English companion, so the rules that are easy to get wrong stay next to the commands.

The longer Chinese handbook is [使用手册.md](./使用手册.md). Architecture and constraints are in [DESIGN.md](./DESIGN.md).

```bash
cp config.example.yaml config.yaml
./bin/migration --help
./bin/migration run  -c config.yaml --workers 16 --rate-limit 20M
./bin/migration diff -c config.yaml
```

An empty `bucket:` migrates every source bucket. `--bucket NAME` pins one. `run` is scan plus copy. `scan` and `migrate` remain available on their own.

## Commands

| Command | Effect |
| --- | --- |
| `list` | List source buckets. Read-only |
| `run` | Scan, then copy. The command to use |
| `scan` | Inventory into SQLite. Single-threaded. Does not write to the target |
| `migrate` | Copy `pending` and `failed` rows only |
| `status` | Local counters. No S3 calls |
| `diff` | List the source live and `HEAD` the target. Exits 1 on a gap. Never uploads |
| `diff --extra` | Also list keys that exist only on the target. Those keys are not a failure |

Shared flags: `-c/--config` (default `config.yaml`), `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`. Help text is bilingual. Full flag and workflow detail is in the [README](../README.md#commands).

## Knobs

Priority is **CLI > environment > YAML > default**.

| Knob | Flag | Env | Default | Applies to |
| --- | --- | --- | --- | --- |
| Object workers | `--workers` | `WORKERS` | 16 | Copy in `run` / `migrate`, and HEAD in `diff` |
| Parts of one large object | `--multipart-concurrency` | `MULTIPART_CONCURRENCY` | 4 | Multipart upload inside `run` / `migrate` |
| Aggregate source read | `--rate-limit` | `RATE_LIMIT` | unlimited | Object bodies in `run` / `migrate` |

`scan` stays single-threaded: `ListObjectsV2` is sequential, and the rows go into one SQLite file. `--workers` does not speed it up, and `rate_limit` does not apply to `scan`, `diff`, `list`, or `status`.

Peak in-flight requests per cluster are about `workers × multipart_concurrency`. The byte cap does not lower that product. A proxy 503 is a concurrency problem: lower `workers` first, then `multipart_concurrency` for large objects. The default `16 × 4` can fill HAProxy `maxconn` even when `rate_limit` is set.

Zero is not one rule:

- YAML `workers: 0` or an omitted field means 16. The same for `multipart_concurrency` means 4. A negative value is an error.
- `WORKERS=0`, `MULTIPART_CONCURRENCY=0`, `--workers 0`, and `--multipart-concurrency 0` are errors. An explicit zero does not fall back to the default.
- `rate_limit: 0`, `RATE_LIMIT=0`, and `--rate-limit 0` are unlimited at every layer.

`20M` and `20MB` are decimal: 20×1000×1000 bytes/s. `20Mi` and `20MiB` are binary: 20×1024×1024 bytes/s. `30M` and `50M` follow the decimal rule. A bare number is bytes/s. A trailing `/s` is ignored. Case does not matter.

`multipart_part_size` must be at least 5 MiB (5242880). A smaller value fails at config load and is not raised. The 16 MiB default is unchanged. Past 10,000 parts, the tool grows the part size for that object.

To protect a production source, start with `--rate-limit 20M` (or `30M` / `50M`) and watch `migration status` plus the NIC. `status` does not call S3. Lighter starting point while you watch:

```yaml
workers: 2
multipart_concurrency: 2
rate_limit: 20M
```

Tuning ranges, the unit table, and spool-disk notes are in the README under [Concurrency](../README.md#concurrency) and [Rate limit](../README.md#rate-limit).

## Safety rules that the resume path depends on

- Never overwrites. Never deletes. There is no overwrite mode to turn on.
- A target object that already matches the inventory is `done`, not `conflict`. Size must match. ETags are compared only when both sides have one and neither is a multipart ETag (`hash-N`, any value containing `-`). A different size or a comparable ETag is a conflict and is never overwritten. A Ctrl-C after a successful PUT therefore resumes as `done` when the bytes match, instead of a false conflict.
- `skipped` is only a source 404 / `NoSuchKey` from a request that finished. `context.Canceled`, `context.DeadlineExceeded`, and `request canceled` are `failed` (the next `migrate` retries them), even when the error text also contains `NotFound` or `StatusCode: 404`.
- The first Ctrl-C cancels the pool and aborts an in-flight multipart upload without deleting a committed object. For a single bucket, that stop during the copy phase exits 0. A second signal exits 130.
- `done` and `conflict` are terminal across a later scan. `pending`, `failed`, and `skipped` are refreshed and set back to `pending`. `ListObjectsV2` cannot filter by modification time, so a catch-up is a full list plus `migrate`.

State, `diff` output, the conflict-log format, and the FAQ are in the README: [Resume and object state](../README.md#resume-and-object-state), [diff](../README.md#checking-the-target-with-diff), [Troubleshooting](../README.md#troubleshooting).

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. A single-bucket copy also exits 0 on the first Ctrl-C. Unfinished rows stay in SQLite |
| 1 | Bad config, a remote error, a failed bucket, a `diff` gap, Ctrl-C during scan, or an interrupt across more than one bucket |
| 130 | Second `SIGINT` or `SIGTERM` |
