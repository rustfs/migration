# How migration copies objects between S3 clusters

`migration` backfills current objects from one S3-compatible cluster to another while applications can continue writing to the destination. The source can be a single RustFS or MinIO instance and the destination can sit behind a load balancer. Transfers go through the migration host because the two clusters have separate object stores. The tool issues no `CopyObject` or `DeleteObject` requests.

The unit of work is one current object in one bucket. The tool preserves its common content headers and user metadata. Historical versions, delete markers, bucket policies, lifecycle rules and Object Lock configuration require separate handling. [Operator guide](../README.md)

## From inventory to copy

```text
                     scan (ListObjectsV2)
source bucket ───────────────────────────────────▶ local SQLite inventory
     │                                                    │
     │ GET or ranged GET                        pending / failed rows
     ▼                                                    ▼
migration workers ── conditional PUT or multipart ──▶ destination bucket
     │                                                    │
     └────────────── update SQLite and conflict log ◀─────┘
```

| Package | Responsibility |
| --- | --- |
| `cmd` | CLI, bucket selection, signals and exit status |
| `internal/config` | YAML, environment and CLI settings, validation and per-bucket paths |
| `internal/s3client` | AWS SDK for Go v2 clients, path-style requests and S3 error classification |
| `internal/cluster` | Bucket discovery and creation, validated and decoded object listings |
| `internal/scanner`, `internal/store` | Build the inventory and persist progress in SQLite |
| `internal/migrator` | Compare keys, transfer bodies, verify writes and log conflicts |
| `internal/ratelimit` | Limit the combined rate of source reads |
| `internal/differ` | Audit the live source and destination using lists and HEAD requests |
| `internal/fakes3` | In-process S3 HTTP server used by regression tests |

`run` scans and then copies one bucket at a time. `scan` and `migrate` expose the two phases independently. A scan checks that the source bucket exists, requests URL-encoded keys with `ListObjectsV2` and validates the returned bucket, keys, sizes and continuation tokens. Pages are read sequentially. The scanner writes batches of up to 500 rows in transactions and marks the scan complete after the last batch is committed. During the copy phase the destination bucket is created if missing. Existing bucket configuration is preserved.

## Local state

SQLite uses the pure Go `modernc.org/sqlite` driver in WAL mode. Rows store the key, size, last-modified time, ETag, status, attempt count, last error and update time. State writes share one database connection while S3 requests proceed in parallel. Use one migration process per state file.

Each database records the bucket and cluster endpoints. A scan binds the source endpoint and copying binds the destination. Older databases without these fields adopt the configured addresses on first use. An operator must check which clusters such a database belongs to before using it again. A missing inventory or a scan marked incomplete prevents `migrate` from starting. Databases from versions without a scan-completion marker can still be resumed. If the old scan may have stopped early, rescan first.

| State | Meaning | Next scan, if the key is listed |
| --- | --- | --- |
| `pending` | Waiting for a copy worker | Refresh the inventory entry |
| `failed` | A transfer, verification or remote request failed | Refresh and return to `pending` |
| `skipped` | The source object disappeared before it could be copied | Refresh and return to `pending` |
| `done` | The new write was verified, or the existing size and ETag matched | Keep the terminal state |
| `conflict` | The destination already contains an object that cannot be confirmed as a match | Keep the terminal state |

New source keys enter the inventory as `pending`. Rescanning means listing the source again because S3 cannot bound a listing by modification time. Reconciliation of a previously terminal key, such as an object removed from the destination later, requires a new inventory in a separate state directory.

## Decisions for each key

1. Issue `HeadObject` on the destination. If the key exists, require an equal size and identical nonempty ETags, including when both ETags are multipart. On a match, mark the row `done`. Otherwise append the conflict to the log and mark the row `conflict`.
2. If the destination reports the key missing, fetch the source. Use its inventoried ETag in `If-Match` when available. A confirmed source-object 404 yields `skipped`. A timeout, cancellation, changed source or failed destination request can be retried.
3. Create a missing key using `If-None-Match: *` on `PutObject` or `CompleteMultipartUpload`. The destination must implement this condition. A HEAD request alone cannot protect against a concurrent writer. If the conditional write loses a race, check the winning object again and classify it under the same match rule. There is no unconditional fallback.
4. Verify the committed size with `HeadObject`, conditioned on the returned write ETag when present. On the single-part path, compare the source and resulting ETags when both are present and comparable. Mark the row `done` after successful verification.

A source and a destination may have identical bytes but different multipart ETags when the part boundaries differ. After a crash between upload completion and the local status update, this can leave a conflict requiring manual review. Comparing the destination's size alone does not authorize marking an existing key `done`.

The conflict log uses three tab-separated fields per record. The first is the object key, followed by the source and destination modification times in RFC3339 UTC. Keys containing control characters or beginning with a quotation mark are written as Go-quoted strings. A mutex serializes appends within the process. If an append succeeds but the database update fails, a retry can produce a second record for that key.

## Transfer paths and resource use

**Single-part uploads.** The worker fetches the source object and streams it to a temporary file in `spool_dir`. It checks the byte count against the inventoried size before rewinding the file for a signed, seekable PUT request. Both paths copy `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` and `x-amz-meta-*` user metadata.

**Multipart uploads.** At or above `multipart_threshold` the worker uses multipart upload. Objects larger than 5 GiB always take this path. A source HEAD establishes the ETag and size. Each ranged GET uses the same ETag in `If-Match`. Before uploading a part, the worker checks the returned ETag, content range and body length. Part numbers determine the order passed to `CompleteMultipartUpload`. On error or cancellation the worker attempts to abort the unfinished upload with a fresh timeout. A process killed before cleanup can leave an incomplete upload on the server.

**Concurrency.** Up to `workers` objects are active, with at most `multipart_concurrency` parts per object. Pending rows are fetched in key order and queued in batches, with a channel of at most 2,048 descriptors. Small transfers need temporary disk space. Multipart workers buffer a part in memory, so the byte footprint grows with part size and the number of active parts. The configured part size ranges from 5 MiB to 5 GiB. It increases automatically to fit within 10,000 parts, or the transfer fails if that would exceed 5 GiB per part.

**Bandwidth.** All source-body reads share the byte limiter, including each ranged read. List, HEAD and upload traffic are outside the limit. The worker and part limits control simultaneous requests separately. The per-host idle connection pool scales with their product, capped at 1,024, to allow connection reuse.

## Failures and signals

Per-object errors are recorded in `last_error`. Workers can continue processing other keys, and the command returns an error when an object failed. A persistence error cancels the worker pool and is returned to the caller. Conflicts and confirmed source deletions have their own states and counts. The first interrupt cancels work and returns a nonzero exit status. A second signal forces an immediate exit with code 130. Restart `migrate` to retry unfinished rows after a completed scan.

## Independent audit

`diff` lists the live source and sends parallel HEAD requests to the destination. It reports missing keys, size mismatches and mismatched comparable ETags. Multipart ETags from different layouts and absent ETags can leave only size to compare. These keys contribute to both `ok` and `size_only`. If the source has no objects, an explicit HEAD checks that the destination bucket exists. A missing destination bucket fails the audit. The audit does not read bodies or change migration state. With `--extra`, a temporary SQLite index of source keys supports a second listing of the destination. Keys found only there do not affect the exit status.

## Verification

`go test -race ./...` exercises Go packages and integration tests against the in-process S3 server, including induced errors and concurrent mutations. The end-to-end test runs the built CLI against independent MinIO and RustFS servers and checks the destination via the S3 API. To run it locally, start Docker, install Go and run the following from the repository root. [Test workflow](../.github/workflows/tests.yml) · [E2E scenarios](../tests/e2e.py)

```bash
python3 -m venv .venv
.venv/bin/python -m pip install boto3==1.42.97
.venv/bin/python tests/e2e.py
```

The packaging workflow builds Linux amd64, Linux arm64 and Windows amd64 archives on matching runners, verifies their SHA-256 digests and starts each packaged executable with `--version`. [Package workflow](../.github/workflows/packages.yml)
