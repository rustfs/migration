# Migration usage

Use a frozen source: stop source writes and deletes, including lifecycle deletion, until final verification and cutover complete. `--source-frozen` acknowledges this external action; it does not lock S3. Uncoordinated dual-cluster deletes are unsupported. Business deletes of inventory keys on the target must also be paused or coordinated through tombstones; freezing only source DELETE requests is insufficient.

The target must support atomic `If-None-Match: *` on PutObject and CompleteMultipartUpload. Verify your actual server version before production use. The tool never calls DeleteObject or CopyObject.

```sh
go build -o bin/migration .
cp config.example.yaml config.yaml
# Set endpoints, credentials and an independent state directory.
# Freeze source mutations externally, then:
./bin/migration run -c config.yaml --source-frozen --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff -c config.yaml
```

Build with Go 1.26+; go.mod selects the patched go1.26.10 toolchain. Configuration precedence is CLI, environment, YAML, defaults. Unknown YAML fields fail. Source and target support session_token / OLD_SESSION_TOKEN / NEW_SESSION_TOKEN for temporary credentials.

`scan` inventories objects and records a completed scan generation. `migrate --source-frozen` retries pending/failed objects only after a complete scan. `run` does both. `list` and `status` do not transfer object bodies.

Every copied object is read back from the destination and checked with SHA-256. An existing same-size object is only done after full source/target digest comparison; different bytes or metadata remain conflicts. ETags are opaque conditional-read tokens, not assumed MD5 checksums. Missing identity or unreadable content fails verification.

`diff` verifies bucket existence, object sizes and full-body SHA-256, and compares preserved metadata. It requires read permissions on both sides and consumes read bandwidth. `--extra` additionally reports target-only keys without treating them as a migration failure. The rate limit applies to source body reads in run/migrate, including the extra full-source digest pass used by multipart copies; it does not limit diff or destination reads/writes.

For an automated gate, `run --strict` and `migrate --strict` return an error if the state contains any pending, failed, conflict or skipped objects, including outcomes from earlier runs. `diff --strict` also checks target-only objects and fails when any exist. Strict mode does not replace a freeze window or final content verification.

`diff --issues-jsonl /path/to/new-file.jsonl` streams all issues as JSON lines containing bucket, key, kind and detail. It refuses to overwrite an existing file. Include `--extra` or `--strict` to record target-only keys. An error or interruption can leave a partial file, so check the command result before treating it as complete.

Errors, partial scans and cancellation return nonzero. Review conflicts and skipped source keys even when a copy command exits successfully; only an independently reviewed final verification is a cutover gate.

State binds source/target endpoints and bucket. Do not repoint a state DB to another cluster. Legacy unbound state with processed rows is rejected: choose a new state directory, rescan and let the tool verify existing target objects. All-bucket mode preserves your basename: job-a.db becomes job-a-photos.db. A process lock prevents concurrent writers while status is read-only. Keep DB, WAL and lock files intact while running.

Parts spool to disk with strict range and length checks. `spool_max_bytes` defaults to 1 GiB and bounds concurrent reservations in one migration, including one extra byte per file for length checks. A single object or part larger than the budget fails; increase the budget or reduce the part size/threshold. This is not a disk quota: files left by crashes, other processes, and other directory contents are excluded. After confirming no jobs are active, manually clean stale files from a dedicated spool directory; monitor free space. Resume retries whole objects; interrupted uploads after SIGKILL require the target's incomplete-multipart lifecycle cleanup. Source changes invalidate inventory rows and may produce conflicts; this is not continuous synchronization.

Only current object data, common content headers and user metadata are migrated. Versions, delete markers, bucket policies, tags, ACLs, storage class, Object Lock, retention/legal hold and source encryption settings are outside scope.

## Testing an explicit RustFS release

```sh
go test ./...
go test -race ./...
go vet ./...
python3 scripts/audit-local-rustfs.py \
  --rustfs-binary /absolute/path/rustfs --expect-version 1.0.4
```

Replace `/absolute/path/rustfs` with the absolute path to the release executable. Before starting either node, the launcher runs that exact file with `--version` and requires an exact match with `1.0.4`. It never falls back to a RustFS executable found on PATH. Both isolated loopback nodes use the verified file, and their processes and temporary data are cleaned up afterward.

The launcher records the executable path, version output and binary SHA-256 in `audit-results/rustfs-1.0.4/rustfs-binary.json`, with node logs beside it; `--output-dir` selects another evidence directory. The separate `real_s3` suite includes full-content verification and interrupted multipart migration followed by recovery from reopened SQLite. These are test scenarios, not a claim that this release has passed. Actual results must be recorded after running the suite against the identified binary.

Earlier results for `1.0.0-alpha.61` remain historical evidence for that release only. They do not establish either success or failure for `1.0.4`.

See [README](../README.md) for configuration, tuning and limits, [design](DESIGN.md) for implementation, and [fix validation](FIXES-2026-10-29.md) for the tested version and remaining compatibility constraints.
