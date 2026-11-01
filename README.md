# migration

在两个 S3 兼容集群之间执行可恢复的单向对象迁移。源端必须在迁移和验收期间冻结写入与删除；目标端必须支持 `PutObject` 和 `CompleteMultipartUpload` 的原子 `If-None-Match: *` 条件写。

迁移不调用 DeleteObject 或 CopyObject。已有目标只在两端内容 SHA-256 一致时记为完成，否则报告冲突；不会主动覆盖冲突对象。此保证依赖目标端正确实现 S3 条件写语义，部署前必须验证对应服务版本。**不支持未经协调的在线双端删除**。迁移清单对象在目标端的业务删除也必须暂停或通过删除墓碑协调；仅禁止旧集群删除不能阻止历史对象被重新写入目标。客户端无法在两个独立集群之间建立原子删除事务。

中文手册：[docs/使用手册.md](docs/使用手册.md) · English guide: [docs/USAGE.md](docs/USAGE.md) · Design: [docs/DESIGN.md](docs/DESIGN.md)

## 构建

```sh
go build -o bin/migration .
# 或 make build
```

需要 Go 1.26 或更高版本；go.mod 指定已修补的 `go1.26.10` 工具链。SQLite 为纯 Go 实现，不需要 CGO。

## 安全使用

1. 在应用、权限策略或存储快照层冻结源桶的写入和删除，包括生命周期删除，保持到最终验收完成。
2. 为每次迁移使用独立状态目录；确认目标实现原子条件写，配置两端地址、区域和最小必要权限。
3. 执行 `run --source-frozen`；该参数只是确认外部已冻结源端，**不会自动冻结 S3**。
4. 检查失败、冲突和跳过记录，执行 `diff` 完整读回校验，再按业务切换方案处理流量。

```sh
cp config.example.yaml config.yaml
# 编辑 endpoint/凭证/独立 db_path，并在应用或存储端冻结源写入及删除
./bin/migration list -c config.yaml
./bin/migration run -c config.yaml --source-frozen --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff -c config.yaml
```

最小配置：

```yaml
old:
  endpoint: https://old.example.com
  access_key: OLD_AK
  secret_key: OLD_SK
  region: us-east-1
new:
  endpoint: https://new.example.com
  access_key: NEW_AK
  secret_key: NEW_SK
  region: us-east-1
bucket: photos                 # 空表示所有源桶；未知 YAML 字段会报错
source_frozen: false            # 已在外部冻结源写入/删除后才可设 true
# 每个迁移任务使用独立目录
db_path: ./state/job-1/migration.db
conflict_log: ./state/job-1/conflict.log
spool_dir: ./state/job-1/spool
spool_max_bytes: 1073741824
workers: 16
multipart_threshold: 52428800
multipart_part_size: 16777216
multipart_concurrency: 4
rate_limit: 0
```

YAML、环境变量、命令行优先级：CLI > 环境变量 > YAML > 默认值。`source_frozen` 可由 `SOURCE_FROZEN=true` 或 `--source-frozen` 覆盖。两端支持 `session_token`，环境变量为 `OLD_SESSION_TOKEN` / `NEW_SESSION_TOKEN`。

其他环境变量：`OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION`，`NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION`，`REGION`，`BUCKET` `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` `WORKERS` `MULTIPART_CONCURRENCY` `RATE_LIMIT`。

## 命令和退出状态

| 命令 | 行为 | 目标写入 |
| --- | --- | --- |
| list | 列出源桶 | 无 |
| scan | 完整扫描源清单到 SQLite | 无 |
| run | 扫描完成后迁移 | 条件创建对象和必要的桶 |
| migrate | 使用已有完整清单重试 pending/failed | 条件创建对象和必要的桶 |
| status | 只读本地状态，不创建缺失 DB | 无 |
| diff | 检查两端桶，并完整读取等尺寸对象计算 SHA-256 | 无 |
| diff --extra | 另外报告仅目标存在的 key | 无 |

`run` / `migrate` 未确认 source_frozen 时拒绝执行。对象传输失败、取消、持久化失败、未完成扫描和状态读取错误都返回非零退出码。`diff` 对缺失、大小差异、SHA-256 差异或无法验证返回失败；`--extra` 报告的目标独有对象允许存在，不单独令命令失败。

conflict 表示保留目标现有数据，skipped 表示源返回明确 NoSuchKey；它们需要操作员审核，不能仅凭 `run` 退出 0 判定全部数据已迁移。未做完整扫描的 `migrate` 不再当作空任务成功。

自动化验收可使用 `run --strict` 或 `migrate --strict`：只要状态库中仍有 pending、failed、conflict 或 skipped，命令就返回非零，包括前次运行遗留的状态。`diff --strict` 同时检查目标独有对象，并在发现时返回非零。严格模式仍不能代替冻结窗口和最终内容验收。

`diff --issues-jsonl /path/to/new-file.jsonl` 将全部差异逐条写入新文件，字段为 bucket、key、kind 和 detail；文件已存在时拒绝覆盖。目标独有 key 需要同时使用 `--extra` 或 `--strict` 才会记录。任务出错或中断时文件可能只包含部分结果，必须结合命令退出状态判断是否完整。

## 完整性与恢复

- 清单记录源大小、ETag 和时间；迁移重新检查源身份，并用 If-Match 和可用的 VersionId 固定读取。ETag 是并发条件，不被当作通用 MD5。
- 小对象先落临时文件；大对象按范围读取并校验 Content-Range、ETag 和精确长度，每片也落临时文件，避免将整个分片保留在内存。
- 新对象提交后从目标读回并比较 SHA-256。已有同尺寸目标也完整读取两端校验，不再把 multipart ETag 或缺失 ETag 降级为“大小一致即完成”。
- 分片对象额外顺序读取一遍源以计算完整摘要。单对象正常复制约读取源一遍、目标一遍；分片复制约读取源两遍、目标一遍。源摘要读取也受 rate-limit 限制。
- SQLite 绑定源/目标 endpoint 和 bucket；不允许更换 endpoint 复用清单。扫描记录 generation，中断扫描使完成凭证失效。
- 有已处理记录而无身份的旧库拒绝自动接管。升级时使用新状态目录重新扫描；已有目标将重新完整验证，无须删除目标对象。
- 单写者锁拒绝多进程同时使用同一个状态库；只读 status 可以并行运行。保留 `.db`、`-wal`、`-shm` 及 `.lock` 文件，不要在任务运行时手工删除锁文件。
- 全桶模式保留任务文件名：`job-a.db` 派生 `job-a-photos.db`，不会和 `job-b.db` 的桶清单混用。
- 源同 key 的身份变化会重新进入待验证状态；已有目标仍不被覆盖。这不是持续同步工具。

恢复是对象级重试。kill -9 后未完成分片可能留在服务端，应配置清理未完成分片的生命周期；工具不持久化分片 uploadId。不能在最后一次校验后允许数据任意变化、再把旧校验当成切换依据。

## 并发与限速

`workers` 默认 16，控制同时处理的对象；`multipart_concurrency` 默认 4，控制单个大对象分片数。workers 上限 1024，分片并发上限 64，乘积上限 4096。不要用最大值作为生产默认。

`rate_limit` 是 run/migrate 的所有源正文读取共享速率，不限制目标上传/读回，也不限制 diff。`20M` / `20MB` 为 20,000,000 B/s；`20MiB` 为 20×1024×1024 B/s；`0` 为不限速。**diff 会完整读取两端数据**，需要安排网络和读负载窗口。

分片大小为 5MiB 到 5GiB；工具可为超过 10,000 片的对象增大片大小，并拒绝超出分片限制的对象。`spool_max_bytes` 默认 1 GiB，限制当前迁移任务并发临时文件预留总量（含每个文件用于检测超长正文的 1 字节）；超过预算的单个对象或分片会失败，需提高预算或降低分片大小/分片阈值。该限制不包含异常退出遗留文件、其他进程和目录内容，也不保证磁盘剩余空间。独立目录中断后确认没有运行中的任务，再人工清理残留文件；监控磁盘并使用目标端分片生命周期清理。

客户端使用 HTTP/1，响应头等待上限 30 秒，单次阻塞正文读取和连接写入上限 90 秒；限速造成的两次读取之间等待不会触发该超时。网络失败按失败对象保留以便重试。

## 能力边界

迁移当前对象内容、常见内容头和用户 metadata。不会复制版本历史、删除标记、桶策略、生命周期、对象 tags、ACL、Object Lock/retention/legal hold、存储等级或源 SSE 配置。目标自身默认加密策略仍生效。使用这些功能的业务需要独立迁移方案，不应直接以本工具完成桶级功能迁移。

目标端不支持条件写、无法返回稳定对象身份、或不能读回内容时不得作为可靠迁移目标。不要通过取消 checksum 校验或改成只比较大小绕过问题。

## 测试

```sh
go test ./...
go test -race ./...
go vet ./...
# 可选：显式选择 RustFS 1.0.4 二进制，启动两个临时回环实例
python3 scripts/audit-local-rustfs.py \
  --rustfs-binary /absolute/path/rustfs --expect-version 1.0.4
```

将 `/absolute/path/rustfs` 替换为待测二进制的绝对路径。启动器先执行该文件的 `--version`，版本必须与 `1.0.4` 精确一致，否则在启动实例前退出；不会从 PATH 自动选择另一个 RustFS。它记录二进制路径、版本输出和 SHA-256，使用同一文件启动两个临时回环实例，结束后清理进程与临时数据。默认证据目录为 `audit-results/rustfs-1.0.4/`，其中 `rustfs-binary.json` 标识实际受测二进制；可用 `--output-dir` 指定其他目录。

审计发现的确定性场景现已纳入默认回归测试，不再需要 audit build tag。`real_s3` 测试保持独立，必须实际执行后才能得出对应服务版本的结论。旧 RustFS `1.0.0-alpha.61` 的分片 ETag 失败仅保留为历史记录，不能当作 `1.0.4` 的通过或失败结果；上线前应对拟部署版本运行真实服务测试。

历史审计：[2026-10-29 审计](docs/AUDIT-2026-10-29.md)。修复验证结果见 [修复记录](docs/FIXES-2026-10-29.md)。

## English summary

This is a resumable one-way migration tool for a **frozen source**. Freeze source writes and deletes externally, then acknowledge with `--source-frozen`. The destination must implement atomic `If-None-Match: *` for both PutObject and CompleteMultipartUpload. Existing objects are read and compared with full SHA-256; mismatches remain conflicts. Copy failures and cancellation return errors. `diff` reads full bodies on both clusters and requires GetObject permissions and bandwidth. See the [English guide](docs/USAGE.md) for migration and upgrade steps.
