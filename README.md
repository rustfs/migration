# migration

**English** | [简体中文](README_ZH.md) | [Deutsch](README_DE.md) | [Español](README_ES.md) | [Français](README_FR.md) | [日本語](README_JA.md) | [한국어](README_KO.md) | [Português](README_PT.md) | [Русский](README_RU.md)

`migration` copies current objects from one S3-compatible cluster to another. In the configuration, `old` is the source and `new` is the destination. The tool tracks progress in local SQLite files and retries at the object level.

**Freeze source writes and deletes externally before scanning and keep them frozen through final verification and cutover.** This includes lifecycle deletion. Pause business deletion of inventory keys on the target as well, or coordinate it through reliable tombstones. The tool cannot provide an atomic delete transaction across clusters. `--source-frozen` is an operator assertion; it does not freeze S3.

The destination must honor atomic `If-None-Match: *` on both `PutObject` and `CompleteMultipartUpload`. Verify that the actual server version supports these conditions. The tool does not call DeleteObject or CopyObject. Existing target objects are marked done only when their full content SHA-256 and preserved metadata match; otherwise they remain conflicts. This safety guarantee depends on the destination implementing conditional writes correctly.

## Documentation

[Command reference](docs/USAGE.md) · [Operator handbook (Chinese)](docs/使用手册.md) · [Design](docs/DESIGN.md) · [Historical audit](docs/AUDIT-2026-10-29.md) · [Fix validation](docs/FIXES-2026-10-29.md)

The translated READMEs are historical and may contain outdated details; follow this README and the command reference for the current safety contract.

## Quick start

Build with Go 1.26 or later; `go.mod` selects the patched `go1.26.10` toolchain. SQLite uses a pure Go implementation and does not require CGO.

```sh
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
# or: make build
cp config.example.yaml config.yaml
```

Edit the endpoints, credentials and independent state directory in `config.yaml`. Freeze source writes and deletes externally and coordinate target deletes before proceeding.

```sh
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --source-frozen --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Review failures, conflicts and skipped objects, run `diff` while both sides are stable, and follow your application's cutover plan. A successful `run` alone does not certify that every object was migrated.

Leave `bucket` empty to select every source bucket; set it or use `--bucket photos` to select one. Explicit `--bucket ""` selects all buckets even when YAML or `BUCKET` specifies one.

## Install

Choose the archive for your platform under **Artifacts** in a successful [Build packages run](https://github.com/rustfs/migration/actions/workflows/packages.yml). Verify the archive against its included SHA-256 digest before extracting it. Linux packages use `.tar.gz` (amd64 or arm64); Windows uses `.zip` (amd64) and the executable is `migration.exe`.

Protect configuration credentials and state files. The process logs endpoints and object keys; endpoint URLs cannot contain credentials, query strings or fragments. Use HTTPS across untrusted networks.

## Configuration

```yaml
old:                              # source, read-only
  endpoint: https://old.example.com
  access_key: OLD_AK
  secret_key: OLD_SK
  region: us-east-1
new:                              # target, read and write
  endpoint: https://new.example.com
  access_key: NEW_AK
  secret_key: NEW_SK
  region: us-east-1
bucket: photos                    # empty selects every source bucket
source_frozen: false              # set true only after freezing externally
# Use an independent directory per migration job.
db_path: ./state/job-1/migration.db
conflict_log: ./state/job-1/conflict.log
spool_dir: ./state/job-1/spool
spool_max_bytes: 1073741824       # 1 GiB per migration task
workers: 16
multipart_threshold: 52428800     # 50 MiB
multipart_part_size: 16777216     # 16 MiB
multipart_concurrency: 4
rate_limit: 0                     # unlimited source reads
```

Unknown YAML fields are rejected. Configuration precedence is CLI > environment > YAML > defaults. `source_frozen` can also be set with `SOURCE_FROZEN=true` or `--source-frozen`. Both endpoints support `session_token` for temporary credentials, with `OLD_SESSION_TOKEN` and `NEW_SESSION_TOKEN` environment overrides.

Other environment overrides are `OLD_ENDPOINT`, `OLD_AK`, `OLD_SK`, `OLD_REGION`, `NEW_ENDPOINT`, `NEW_AK`, `NEW_SK`, `NEW_REGION`, `REGION`, `BUCKET`, `DB_PATH`, `CONFLICT_LOG`, `SPOOL_DIR`, `WORKERS`, `MULTIPART_CONCURRENCY` and `RATE_LIMIT`. The destination account needs `s3:CreateBucket` if the tool must create a bucket. `list` and `scan` require the source endpoint; `run`, `migrate` and `diff` require both endpoints.

## Commands and exit status

| Command | Behavior | Writes to target |
| --- | --- | --- |
| `list` | List source buckets | No |
| `scan` | Scan the complete source inventory into SQLite | No |
| `run` | Scan, then migrate | Conditionally creates objects and required buckets |
| `migrate` | Retry pending/failed objects from a completed scan | Conditionally creates objects and required buckets |
| `status` | Read local state without creating a missing database | No |
| `diff` | Verify buckets, size, preserved metadata and full-body SHA-256 | No |
| `diff --extra` | Also report target-only keys | No |

`run` and `migrate` reject execution without the source-freeze assertion. Transfer failures, cancellation, persistence failures, incomplete scans and state-read errors return nonzero. `diff` returns nonzero for missing, mismatched or unverifiable objects. With `--extra`, target-only keys are reported but do not independently cause failure.

A conflict preserves an existing target object; skipped means the source returned a definite `NoSuchKey`. Both require review, even when `run` exits 0. For an automated gate, `run --strict` and `migrate --strict` return nonzero if the state contains pending, failed, conflict or skipped objects, including results from prior runs. `diff --strict` also reports and fails on target-only keys. Strict mode does not replace a freeze window and final content verification.

`diff --issues-jsonl /path/to/new-file.jsonl` streams every issue as JSON Lines with `bucket`, `key`, `kind` and `detail`. Existing files are not overwritten. Add `--extra` or `--strict` to include target-only keys. On interruption or error the file may be incomplete; check the command result before using it for acceptance.

## Integrity and recovery

- The inventory records source size, ETag and modification time. Reads use If-Match and an available VersionId to pin the representation. ETags are conditional-read tokens, not assumed to be MD5 checksums.
- Small objects spool to disk; large objects use bounded range reads with Content-Range, ETag and exact-length checks. Each part also spools to disk.
- New objects are read back from the target and checked against SHA-256. Existing same-size objects are fully read on both sides; equal size or a multipart ETag alone does not establish equality. Preserved headers and user metadata are also compared.
- A normal single-object copy reads the source once and the target once. Multipart copies read the source approximately twice and the target once to verify the assembled object. Source digest reads are included in the rate limit.
- SQLite state binds the source and target endpoints and bucket. Do not reuse a database with different endpoints or repoint an endpoint to a different cluster or account. A scan generation records completion; an interrupted rescan invalidates that certificate.
- Legacy unbound state with processed rows is rejected. Use a new state directory and rescan after upgrading; existing target objects will be fully verified without deleting them.
- A single-writer lock prevents simultaneous processes using the same state database; read-only `status` can run concurrently. Keep the `.db`, `-wal`, `-shm` and `.lock` files intact during a job. In all-bucket mode `job-a.db` derives names such as `job-a-photos.db`.
- A changed source identity re-enters validation; the tool does not overwrite an existing target and is not a continuous synchronization system.

Recovery retries whole objects, not individual parts. A forced termination can leave unfinished multipart uploads on the server; configure a lifecycle rule to clean them up. Do not treat an old verification as valid after allowing data to change.

## Concurrency, rate and disk limits

`workers` defaults to 16 and controls concurrent objects; `multipart_concurrency` defaults to 4 and controls concurrent parts per object. Their limits are 1024 and 64 respectively, with a product limit of 4096. Do not use these maxima as production defaults. Scan is single-threaded.

`rate_limit` caps aggregate source-body reads for `run` and `migrate`, including the multipart digest pass. It does not limit target uploads or readback, or `diff`. `20M` / `20MB` means 20,000,000 B/s; `20MiB` means 20 × 1024 × 1024 B/s; `0` means unlimited. `diff` reads both full bodies for equal-sized objects, so plan for the bandwidth and read load.

Parts range from 5 MiB to 5 GiB. The tool may increase part size to stay within 10,000 parts and rejects objects that exceed the multipart limits. `spool_max_bytes` defaults to 1 GiB and limits concurrent temporary-file reservations within one migration, including one extra byte per file for length checks. A single object or part larger than the budget fails: increase the budget or reduce the part size or threshold.

The budget does not include files left by crashes, other processes or other directory contents, and does not guarantee free disk space. Use a dedicated spool directory and monitor disk capacity. After confirming no job is active, manually remove stale temporary files. The client uses HTTP/1 with a 30-second response-header timeout and a 90-second timeout for an individual blocked body read or connection write; rate-limit waits between reads do not trigger that timeout.

## Scope

The tool migrates current object contents, common content headers and user metadata. It does not copy version history, delete markers, bucket policies, lifecycle rules, object tags, ACLs, storage class, Object Lock, retention/legal hold or source SSE configuration. The target's default encryption policy still applies. Workloads that depend on these features need a separate migration plan.

A destination that does not support conditional writes, cannot provide stable object identities or cannot serve readback is not suitable for reliable migration with this tool. Do not bypass verification with size-only comparison.

## Testing

```sh
go test ./...
go test -race ./...
go vet ./...
# Optional: start two temporary loopback instances with the specified RustFS binary.
python3 scripts/audit-local-rustfs.py \
  --rustfs-binary /absolute/path/rustfs --expect-version 1.0.4
```

The launcher checks that the specified executable reports the expected version, records its path, version and SHA-256, and stops the temporary instances and removes their data afterward. It does not choose another executable from PATH. The default evidence directory is `audit-results/rustfs-1.0.4/`; `--output-dir` selects another location. Real-service results apply only to the version actually tested; validate the server versions intended for deployment.

## License

Licensed under the [Apache License 2.0](LICENSE).
