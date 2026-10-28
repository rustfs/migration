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
  3. migration run  -c config.yaml       # scan + copy / 扫描并复制
  4. migration diff -c config.yaml       # verify no gaps / 核对有无遗漏

Leave bucket empty in YAML to migrate every bucket on the source.
YAML 里 bucket 留空，就会迁移来源上的每一个桶。`

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

Examples:
  migration run -c config.yaml
  migration run -c config.yaml --bucket photos`

const scanShort = `Inventory source objects into SQLite / 扫描源对象到本地 SQLite`
const scanLong = `Walk ListObjectsV2 on the source and upsert {key,size,mtime,etag} locally.
Re-running scan picks up new keys and retries skipped/failed rows.
Does not write to the target.

对来源做 ListObjectsV2，把 {key,size,mtime,etag} 写入本地 SQLite。
再次扫描会收录新对象，并让 previously skipped/failed 的行重新进入 pending。
不会对目标写入。

Example:
  migration scan -c config.yaml`

const migrateShort = `Copy pending objects (never overwrites) / 复制待迁移对象（永不覆盖）`
const migrateLong = `Copy every SQLite row in pending/failed. If the key already exists on
the target and matches the inventoried size (and single-part ETag, when both
sides have one), it is marked done. A different object is logged to
conflict.log and left untouched.

复制 SQLite 中 pending/failed 的对象。目标已有同名 key 且与清单大小一致
（两边都有非分片 ETag 时 ETag 也须一致）则记为 done。不一致则写入
conflict.log，不覆盖。

Example:
  migration migrate -c config.yaml`

const statusShort = `Show local progress counters / 显示本地进度`
const statusLong = `Print per-bucket counters from the local state DB(s). No S3 calls.
打印本地状态库中的每桶计数，不访问 S3。

Example:
  migration status -c config.yaml`

const diffShort = `Compare source vs target / 对比源与目标`
const diffLong = `List the source and HEAD each key on the target. Reports missing objects
and size/ETag mismatches. Exit code 1 if the source is not fully present.

列出源对象并对目标做 HEAD：报告缺失、大小/ETag 不一致。
源数据未完整出现在目标上时退出码为 1。

Examples:
  migration diff -c config.yaml
  migration diff -c config.yaml --extra     # also list keys only on target
                                            # 同时列出仅存在于目标上的 key`

const exampleFooter = `
Config / 配置:  -c config.yaml   (default ./config.yaml)
Bucket:         --bucket NAME    overrides YAML; empty YAML bucket = all buckets
                                覆盖 YAML；YAML 里 bucket 为空表示全部桶
Env overrides / 环境变量覆盖:
  OLD_ENDPOINT OLD_AK OLD_SK OLD_REGION
  NEW_ENDPOINT NEW_AK NEW_SK NEW_REGION
  BUCKET DB_PATH CONFLICT_LOG SPOOL_DIR REGION`
