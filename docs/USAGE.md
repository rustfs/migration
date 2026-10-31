# Running a migration

Use `run` for the first transfer and `diff` for an independent check of the destination. The full operator guide covers installation, permissions, configuration, concurrency and troubleshooting. [English guide](../README.md) · [中文使用手册](使用手册.md)

Copy `config.example.yaml` to `config.yaml`. Set `old` to the source and `new` to the destination. The destination must honor `If-None-Match: *` on both `PutObject` and `CompleteMultipartUpload`. This protects an object created by another writer between the initial check and the upload.

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --bucket photos --rate-limit 20M
./bin/migration status -c config.yaml --bucket photos
./bin/migration diff   -c config.yaml --bucket photos
```

Omit `--bucket photos` and leave `bucket` empty in the configuration to process every source bucket. An explicit `--bucket ""` also selects all buckets. Keep the bucket selection and the state directory the same when resuming. For a single bucket the tool uses `db_path`. All-bucket mode uses `migration-<bucket>.db` files alongside `db_path`.

The source account must be able to list and read the selected bucket. The destination account needs permission to check keys and write objects, including multipart uploads and aborts. On Amazon S3, `s3:ListBucket` lets a HEAD request distinguish a missing key (404) from a permission error (403). The tool treats a permission error as a failure. [S3 HEAD permissions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html)

## Pick a command

| Command | When to use it |
| --- | --- |
| `list` | Check which buckets the source account can list. |
| `scan` | Save a full inventory in SQLite before you start copying. The source is read sequentially, one listing page at a time. |
| `migrate` | Copy the `pending` and `failed` rows from an existing inventory. If a scan failed or was interrupted, complete another scan first. |
| `run` | Scan and copy in one command. Run it again to pick up keys added since the previous scan. |
| `status` | Read object counts from the local state files. This does not contact S3. |
| `diff` | Check which source keys are missing or have different sizes or comparable ETags at the destination. |
| `diff --extra` | Also list keys found only at the destination. These do not make the command fail. |

## Resume a transfer

After an interruption during copying, run `migrate` again. It checks existing keys before writing. A matching size and identical nonempty ETags mark an existing object `done`. Any other existing object is recorded as `conflict` and preserved. Even identical multipart content can have different ETags when the part sizes differ, so review conflicts left by an interrupted upload. If the source has changed since the inventory was built, use `run` to refresh the inventory before retrying.

Inspect `last_error` on `failed` rows. A confirmed missing source key is `skipped` and can be picked up if it reappears on a later scan. Both `done` and `conflict` are terminal. If an object was subsequently removed from the destination, start a new inventory in a separate state directory to reconsider it. A database belongs to one bucket and one pair of endpoints. Confirm the cluster addresses before opening a database created by an older version.

`diff` reads current listings and object headers independently of the inventory. Its `size_only` counter is included in `ok` and identifies objects for which only the size could be checked. For an empty source bucket, `diff` checks that the destination bucket exists. A missing destination bucket also yields exit code 1. A byte-for-byte audit requires reading object bodies separately.

## Set concurrency and bandwidth

| Setting | Default | Effect |
| --- | --- | --- |
| `--workers` / `WORKERS` | 16 | Concurrent copies, or concurrent HEAD requests for `diff` |
| `--multipart-concurrency` / `MULTIPART_CONCURRENCY` | 4 | Concurrent parts per large object |
| `--rate-limit` / `RATE_LIMIT` | Unlimited | Combined bandwidth for reading source object bodies |

The CLI takes precedence over the environment, which takes precedence over YAML. In YAML, setting either concurrency value to zero selects its default. On the command line and in the environment, concurrency must be at least one. A zero rate limit always means unlimited. `20M` means 20,000,000 bytes/s and `20MiB` means 20,971,520 bytes/s. Uploads consume bandwidth in addition to source reads. [Configuration and tuning](../README.md#concurrency)

## Exit status

| Code | Meaning |
| --- | --- |
| 0 | The command completed. Check `conflict` and `skipped` counts for objects that may need attention. |
| 1 | The operation failed or was interrupted, at least one copy failed, or `diff` found missing or mismatched keys. |
| 130 | A second interrupt forced an immediate exit. |
