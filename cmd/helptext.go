package cmd

const Version = "0.2.0"

const rootShort = `Safe one-way S3 migration / 安全的单向 S3 迁移`

const rootLong = `migration copies objects from a source S3-compatible cluster to a target.
Never overwrites. Never deletes. Resume after Ctrl-C via local SQLite.

migration 把对象从来源 S3 兼容集群复制到目标集群。
永不覆盖、永不删除；用本地 SQLite 记录进度，Ctrl-C 后续跑即可。

Quick start / 快速开始:
  1. cp config.example.yaml config.yaml
  2. Fill in old/new endpoints and keys  （填写来源/目标地址和密钥）
  3. Freeze source writes/deletes, then confirm --source-frozen / 冻结源写入和删除后确认
     migration run -c config.yaml --source-frozen
  4. migration diff -c config.yaml       # verify no gaps / 核对有无遗漏

Leave bucket empty in YAML to migrate every bucket on the source.
YAML 里 bucket 留空，就会迁移来源上的每一个桶。

Concurrency / 并发:
  workers                 objects copied or verified at once (default 16)
                          同时复制（diff 时读取两端正文校验）的对象数，默认 16
  multipart_concurrency   parts of one large object in parallel (default 4)
                          单个大对象同时上传的分片数，默认 4
  Scan is single-threaded. Peak HTTP is about workers × multipart_concurrency.
  扫描始终单线程。峰值请求大约是 workers × multipart_concurrency。
  CLI: --workers N   --multipart-concurrency N
  Env: WORKERS       MULTIPART_CONCURRENCY
  Priority / 优先级: CLI > env > YAML > default
                     命令行 > 环境变量 > 配置文件 > 默认值

Bandwidth / 带宽:
  rate_limit              aggregate bytes read from the source per second
                          所有 worker（含大对象分片）从源读取的合计上限
  0 or omit = unlimited. Applies to run / migrate object bodies only.
  0 或省略 = 不限速。只作用于 run / migrate 的对象正文。
  20M / 20MB  = 20×1000×1000 B/s (decimal MB, not 1024)
  20Mi / 20MiB = 20×1024×1024 B/s (binary MiB)
  20M / 20MB 是十进制，每秒 20×1000×1000 字节；20Mi / 20MiB 才是 1024 进制。
  CLI: --rate-limit 20M     Env: RATE_LIMIT
  Same priority / 同样的优先级: CLI > env > YAML > default`

const listShort = `List buckets on the source / 列出源集群上的桶`
const listLong = `List every bucket on the source endpoint (read-only).
只读列出源端点上的全部桶。

Example:
  migration list -c config.yaml`

const runShort = `Scan then copy (recommended) / 扫描并复制（推荐）`
const runLong = `One-step migration: inventory the source, then copy every object that
does not yet exist on the target. Resumable. Never overwrites.

一步完成迁移：先扫描来源清单，再把目标上还不存在的对象复制过去。
可断点续传，永不覆盖。

Freeze source writes and deletes before run/migrate; explicitly set --source-frozen.
执行 run/migrate 前须在外部冻结来源写入和删除，并明确设置 --source-frozen。

--workers controls how many objects are copied at once.
--multipart-concurrency controls parts inside one large object.
--rate-limit caps the total bytes/sec those workers read from the source.
Scan itself stays single-threaded and is not rate limited.

--workers 控制同时复制多少个对象。
--multipart-concurrency 控制单个大对象内部的分片并发。
--rate-limit 限制这些 worker 从源读取的合计字节每秒。
扫描阶段本身是单线程的，也不吃带宽限额。
20M = 20×1000×1000 B/s；20MiB = 20×1024×1024 B/s；0 = 不限速。

Examples:
  migration run -c config.yaml --source-frozen
  migration run -c config.yaml --source-frozen --bucket photos --workers 32
  migration run -c config.yaml --source-frozen --multipart-concurrency 8
  migration run -c config.yaml --source-frozen --workers 16 --rate-limit 20M`

const scanShort = `Inventory source objects into SQLite / 扫描源对象到本地 SQLite`
const scanLong = `Walk ListObjectsV2 on the source and upsert {key,size,mtime,etag} locally.
Re-running scan picks up new keys and retries skipped/failed rows.
Does not write to the target.

对来源做 ListObjectsV2，把 {key,size,mtime,etag} 写入本地 SQLite。
再次扫描会收录新对象，并让 previously skipped/failed 的行重新进入 pending。
不会对目标写入。

Scan is single-threaded. --workers does not speed up listing.
扫描是单线程的（List 分页必须串行），--workers 对 scan 不生效。

Example:
  migration scan -c config.yaml`

const migrateShort = `Copy pending objects (never overwrites) / 复制待迁移对象（永不覆盖）`
const migrateLong = `Copy every SQLite row in pending/failed. If the key already exists on
the target and its full SHA-256 and preserved metadata match the source, it is marked done.
An existing complete scan and --source-frozen are required. A different object is logged to
conflict.log and left untouched.

复制已有完整扫描清单中 pending/failed 的对象，执行前确认 --source-frozen。
目标已有同名 key 且两端正文 SHA-256 和保留的元数据一致则记为 done。不一致则写入
conflict.log，不覆盖。

--workers is the object pool. --multipart-concurrency is per large object.
--rate-limit is the shared source-read cap across that pool (0 = unlimited).
--workers 是对象并发。--multipart-concurrency 是单个大对象的分片并发。
--rate-limit 是这个池子从源读取的合计上限（0 = 不限速）。
20M = 20×1000×1000 B/s；20MiB = 20×1024×1024 B/s。

Example:
  migration migrate -c config.yaml --source-frozen --workers 8 --rate-limit 30M`

const statusShort = `Show local progress counters / 显示本地进度`
const statusLong = `Print per-bucket counters from the local state DB(s). No S3 calls.
打印本地状态库中的每桶计数，不访问 S3。

Example:
  migration status -c config.yaml`

const diffShort = `Compare source vs target / 对比源与目标`
const diffLong = `List the source, HEAD the target, and read both object bodies to compare SHA-256.
Reports missing objects and size/content/preserved-metadata mismatches; gaps or read errors exit 1.
Keep both clusters quiescent during final verification. This reads all data,
requires GetObject on both endpoints, and is not subject to --rate-limit.

列出源对象、对目标做 HEAD，并读取两端完整正文进行 SHA-256 校验。
缺失、大小/内容/保留元数据不一致或读取错误时退出码为 1。最终验收期间两端须保持静止。
需要两端 GetObject 权限，会读取全部数据，当前不受 --rate-limit 限制。

--workers controls parallel object verification. It does not upload anything.
--workers 控制同时校验的对象数，diff 不会上传对象。

Examples:
  migration diff -c config.yaml
  migration diff -c config.yaml --workers 32
  migration diff -c config.yaml --extra     # also list keys only on target
                                            # 同时列出仅存在于目标上的 key`

const exampleFooter = `
Config / 配置:  -c config.yaml   (default ./config.yaml)
Bucket:         --bucket NAME    overrides YAML; empty YAML bucket = all buckets
                                覆盖 YAML；YAML 里 bucket 为空表示全部桶
Concurrency / 并发:
  --workers N                   objects in parallel for run, migrate, diff (default 16)
                                run/migrate/diff 的对象并发，默认 16
  --multipart-concurrency N     parts in parallel inside one large object (default 4)
                                单个大对象的分片并发，默认 4
  Scan is single-threaded / 扫描（scan）始终单线程，不受 --workers 影响
Bandwidth / 带宽:
  --rate-limit SIZE             shared source-read cap for run/migrate (default unlimited)
                                run/migrate 从源读取的合计上限，默认不限速
                                20M/30M/50M = decimal MB/s (×1000×1000)
                                20MiB = binary MiB/s (×1024×1024); 0 = unlimited
                                20M 是十进制 MB/s；20MiB 才是 1024 进制；0 = 不限速
  Priority / 优先级: CLI > env > YAML > default
                     命令行 > 环境变量 > 配置文件 > 默认值
Env overrides / 环境变量覆盖:
  OLD_ENDPOINT OLD_AK OLD_SK OLD_REGION
  NEW_ENDPOINT NEW_AK NEW_SK NEW_REGION
  BUCKET DB_PATH CONFLICT_LOG SPOOL_DIR REGION
  WORKERS MULTIPART_CONCURRENCY RATE_LIMIT SOURCE_FROZEN
  OLD_SESSION_TOKEN NEW_SESSION_TOKEN`
