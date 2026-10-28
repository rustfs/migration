# migration

Safe one-way object migration between two S3-compatible clusters
(RustFS / MinIO / S3). **Never overwrites. Never deletes. Resumable.**

在两个 S3 兼容集群之间做安全的单向对象迁移。**永不覆盖、永不删除、可断点续传。**

Chinese user manual: [docs/使用手册.md](docs/使用手册.md)

```bash
cp config.example.yaml config.yaml   # edit endpoints / 填写地址和密钥
go build -o bin/migration .
./bin/migration run  -c config.yaml  # scan + copy / 扫描并复制
./bin/migration diff -c config.yaml  # verify / 核对有无遗漏
```

```text
./bin/migration --help
```

| Command | English | 中文 |
| --- | --- | --- |
| `list` | List source buckets | 列出源集群的桶 |
| `run` | Scan then copy (recommended) | 一键扫描并复制（推荐） |
| `scan` | Inventory into SQLite | 扫描清单到本地 SQLite |
| `migrate` | Copy pending/failed only | 只复制待处理对象 |
| `status` | Local counters | 查看本地进度 |
| `diff` | Compare source vs target | 对比源与目标 |

Leave `bucket:` empty to migrate **every** source bucket. `--bucket NAME` pins one.

There is no MB/s rate limit (`bandwidth` / `rate_limit` are not settings). Load is `workers` (default 16) and `multipart_concurrency` (default 4); peak is about `workers × multipart_concurrency`. Lower those, then watch `status` and the NIC. Stay under HAProxy `maxconn`. See [docs/使用手册.md](docs/使用手册.md) §9.1 and [docs/USAGE.md](docs/USAGE.md).

没有 MB/s 限速字段。负载由 `workers`（默认 16）和 `multipart_concurrency`（默认 4）决定，峰值大约是二者相乘。要降负载就调小它们，再看 `status` 和网卡，并留出 HAProxy `maxconn` 余量。详见手册 §9.1。

Safety model: `HeadObject` on the target before any write; conflicts go to `conflict.log`. Multipart for large objects. SQLite WAL per object. Re-run after Ctrl-C.

See `migration <command> --help` for bilingual (English + 中文) help.
