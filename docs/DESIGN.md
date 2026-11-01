# Migration design

## Safety model

The source is externally frozen for writes and deletes through scan, migration and verification. SourceFrozen is an explicit operator assertion and does not enforce an S3 lock. Business deletes of inventory keys on the target must also be paused or coordinated through tombstones. Cross-cluster concurrent deletes cannot be made atomic by a client; they are unsupported without application-level coordination.

The destination must correctly implement atomic If-None-Match: * for PutObject and CompleteMultipartUpload. No DeleteObject or CopyObject operations are used. Existing target bytes are preserved on conflict. Compatibility tests against the deployed server version are a prerequisite.

## Inventory and persistence

SQLite holds bucket, normalized source/target endpoint identity, scan generation, completion generation and per-key size/ETag/last-modified/status. The state format distinguishes new databases from legacy unbound checkpoints. Unsafe legacy adoption or endpoint reuse is rejected.

Scanning invalidates completeness before network work, lists using encoding-type=url, decodes keys exactly once, validates pagination and persists batches transactionally. Completion is recorded only after the final successful flush. A partial scan cannot authorize copying. Changed source identity resets terminal rows for revalidation; targets are never overwritten because of a rescan.

A separate SQLite pager lock excludes writers across processes and is released by the OS on process death. Status opens the inventory read-only. The inventory uses WAL and synchronous=FULL with one writer connection. Keep state, sidecar and lock files together during operation.

## Transfer

1. HEAD target; only a structured missing response permits the create path.
2. HEAD source and compare its identity against the inventory. HEAD404 is not enough to declare a source key deleted; a GET must confirm NoSuchKey.
3. If target exists, read both equal-size objects with If-Match, compare full SHA-256 and preserved metadata, then mark done or conflict. ETags are concurrency tokens, not cross-cluster content hashes.
4. Otherwise read source using the pinned ETag/available VersionId. Small objects spool to disk while hashing. Multipart ranges validate Content-Range, Content-Length and ETag, spool to disk and upload through bounded concurrency.
5. Before commit, recheck source identity. Multipart also reads the source sequentially for a complete digest. These checks detect completed mutations but do not replace the external freeze requirement.
6. Commit with If-None-Match: *. A competing target writer produces a conflict; other write errors remain retryable failures.
7. HEAD and read back target with If-Match; compare full SHA-256 before done.

Source and destination errors retain provenance. Only a confirmed source NoSuchKey becomes skipped; destination404, bucket disappearance, network failures, canceled reads and digest failures are failed. Local persistence failures cancel the worker pool. Ordinary object failures do not stop unrelated objects, but the final command returns a failure summary. Cancellation returns an error.

Object-level retries resume pending/failed rows. Multipart upload IDs are not checkpointed; normal failures abort the session with an independent bounded context. Crash leftovers require server lifecycle cleanup. A committed object whose local done update was interrupted is verified on retry.

## Verification

Diff checks both buckets, inventories source keys and validates every equal-size object by complete SHA-256 reads of both endpoints, plus preserved metadata. Missing identity or unreadable data returns an error. Target-only keys are optional informational output.

Workers and producer share cancellation; a fatal HEAD/GET error stops all producers, avoiding blocked sends after workers exit. Each diff run owns a uniquely created temporary database, so concurrent calls cannot delete each other's inventory. Report write errors propagate.

## Resources and transport

Object workers, part concurrency and job queues are bounded. Multipart bodies spool to disk instead of io.ReadAll; range reads are limited to expected bytes plus one. Temporary disk remains a capacity requirement. A shared limiter covers all source body reads in migration, including the extra digest pass. Destination reads/writes and diff are not throttled by this setting.

HTTP/1 connection pools follow worker fan-out. Response headers have a 30-second timeout; each blocked body Read and connection Write has a 90-second deadline. The body watchdog is inactive between reads, so low configured bandwidth does not cause false idle timeouts. Optional SDK checksum extensions are disabled unless required; application SHA-256 validation remains mandatory.

## Scope and testing

The tool migrates current object content, common HTTP content headers and user metadata. Version history, delete markers, tags, ACLs, storage classes, source SSE configuration, Object Lock and bucket configuration require separate migration planning.

Regression tests cover conditional-write races, source mutation/mixed parts, completed deletion before commit, same-size corruption, error provenance, partial scans, cross-endpoint checkpoint reuse, process-lock crash recovery, diff cancellation, encoding and explicit freeze prerequisites. Fake S3 models conditional requests and range headers but is not a full S3 certification suite. The optional `real_s3` harness tests an explicitly selected executable, rather than whichever RustFS version happens to be installed on PATH:

```sh
python3 scripts/audit-local-rustfs.py \
  --rustfs-binary /absolute/path/rustfs --expect-version 1.0.4
```

The executable path must be absolute. The launcher checks that file's `--version` output against the exact expected version before starting either node; a mismatch aborts setup. It records the resolved path, version output and executable SHA-256, then starts both isolated loopback nodes from that same file. The default evidence directory is `audit-results/rustfs-1.0.4/`, including `rustfs-binary.json` and node logs; `--output-dir` can override it. Processes and temporary server data are cleaned up at the end.

The suite exercises full-body verification and recovery after a real UploadPart has been accepted but its response is interrupted. The recovery test checks cancellation, remote abort cleanup, a retryable checkpoint, reopening SQLite without rescanning, and final SHA-256 plus diff results through the unchanged endpoint. Describing these scenarios does not claim a successful `1.0.4` run; that requires actual execution against the recorded binary. Previous `1.0.0-alpha.61` results describe only that historical release.

See [fix validation](FIXES-2026-10-29.md) for actual test results and remaining service compatibility constraints. The earlier audit remains a historical record of its pinned commit.
