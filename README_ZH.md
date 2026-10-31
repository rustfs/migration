# migration

[English](README.md) | **简体中文** | [Deutsch](README_DE.md) | [Español](README_ES.md) | [Français](README_FR.md) | [日本語](README_JA.md) | [한국어](README_KO.md) | [Português](README_PT.md) | [Русский](README_RU.md)

`migration` 用于将当前对象从一个兼容 S3 的集群复制到另一个集群。例如，业务已切换到新集群写入，但仍需从旧的 RustFS 或 MinIO 集群补齐历史数据。配置中的 `old` 为源集群，`new` 为目标集群，两端均采用路径式寻址。

工具会补齐缺失的对象，将同名冲突记入 `conflict.log`，并用本地 SQLite 数据库保存进度。迁移不会删除对象。目标端的 `PutObject` 和 `CompleteMultipartUpload` 都必须支持 `If-None-Match: *` 条件写入。这样，即使业务在迁移检查之后才写入同名对象，也能保留该对象。

除当前对象的内容外，还会复制 `Content-Type`、`Cache-Control`、`Content-Disposition`、`Content-Encoding`、`Content-Language`、`Expires` 和 `x-amz-meta-*` 用户元数据。版本历史、删除标记、对象锁、桶策略和生命周期规则不在迁移范围内。

## 延伸阅读

[中文使用手册](docs/使用手册.md) · [英文命令速查](docs/USAGE.md) · [设计说明（英文）](docs/DESIGN.md)

## 快速开始

以下命令在仓库根目录运行，需要 Go 1.22 或更高版本。复制配置模板后，请先编辑 `config.yaml`，填入两端的地址和凭据。

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
# 或：make build
cp config.example.yaml config.yaml   # 填写来源和目标的地址、密钥
```

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

`bucket:` 留空会迁移源集群中的所有桶。写成 `bucket: photos`，或加上 `--bucket photos`，就只迁这一个桶。

即使 YAML 或 `BUCKET` 已指定某个桶，也可以用 `--bucket ""` 选择源端的全部桶。

`--rate-limit 20M` 表示所有任务合计每秒最多从源端读取 20,000,000 字节。二进制单位及适用范围见[限速](#rate-limit)。

## 使用预编译包

打开 [Build packages](https://github.com/rustfs/migration/actions/workflows/packages.yml)，在成功运行的 **Artifacts** 中选择对应平台。解压下载文件，用随附的 SHA-256 校验值核对软件包，再解开软件包。Linux amd64 和 arm64 使用 `.tar.gz`，Windows amd64 使用 `.zip`，可执行文件为 `migration.exe`。

```bash
./migration --version
```

```powershell
.\migration.exe --version
```

`config.yaml` 包含访问密钥。在 Linux 上可执行 `chmod 600 config.yaml`，仅允许文件所有者读写。运行日志会记录端点和对象键。

## 配置

```bash
cp config.example.yaml config.yaml
```

分别填写两端的 HTTP 或 HTTPS 地址，并在凭据字段中填写访问密钥。端点 URL 不可包含凭据、查询参数或片段。如果需要在目标端创建桶，目标账户还需具备 `s3:CreateBucket` 权限。

```yaml
old:                              # 来源，只读
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # 目标，读 + 写
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # 空 = 源集群中的所有桶
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # 小对象临时目录，默认跟 db 同目录

workers: 16                       # 同时处理的对象数；0 或省略 = 16
multipart_threshold: 52428800     # >= 50 MiB 走分片上传
multipart_part_size: 16777216     # 16 MiB；必须 >= 5 MiB（5242880）
multipart_concurrency: 4          # 单个大对象的分片并发；0 或省略 = 4
rate_limit: 0                     # 源读取合计上限；0 或省略 = 不限速
```

`list` 和 `scan` 只需要源端的地址和凭据。`run`、`migrate` 和 `diff` 需要两端的地址和凭据。

### 优先级

从高到低：

1. 命令行：`--bucket`、`--workers`、`--multipart-concurrency`、`--rate-limit`
2. 环境变量
3. YAML
4. 内置默认值

| 配置 | 省略时的默认值 |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB（52428800） |
| `multipart_part_size` | 16 MiB（16777216） |
| `rate_limit` | 不限速 |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | `db_path` 所在目录 |
| `region` | `us-east-1` |

例子：YAML 里 `workers: 8`，环境变量 `WORKERS=32`，命令行 `--workers 4`，实际使用 **4**。

### 0 表示什么

`0` 在不同字段上不是同一个意思。

| 字段 | YAML 写 `0` 或省略 | 环境变量写 `0` | 命令行写 `0` |
| --- | --- | --- | --- |
| `workers` | 默认 16 | 报错 | 报错（`--workers 0`） |
| `multipart_concurrency` | 默认 4 | 报错 | 报错 |
| `multipart_threshold` | 默认 50 MiB | 没有对应环境变量 | 没有对应参数 |
| `multipart_part_size` | 默认 16 MiB | 没有对应环境变量 | 没有对应参数 |
| `rate_limit` | 不限速 | 不限速（`RATE_LIMIT=0`） | 不限速（`--rate-limit 0`） |

并发或大小写成负数，启动即失败。`WORKERS` 和 `MULTIPART_CONCURRENCY` 必须是纯整数。单位只写在 `RATE_LIMIT` 上：`RATE_LIMIT=20M` 合法，`WORKERS=20M` 不合法。

`multipart_part_size` 的取值范围为 5 MiB 至 5 GiB，默认 16 MiB。超过 5 GiB 的对象始终使用分片上传。每个对象最多拆为 10,000 片，必要时工具会自动增大分片。如果仍需单片超过 5 GiB，迁移会报错。

桶名不能包含空白、控制字符、路径分隔符及 Windows 文件名中的保留符号，也不能是 `.` 或 `..`。

### 环境变量

环境变量非空时覆盖 YAML 中的对应设置。

| 变量 | 覆盖 |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | 来源端点 |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | 目标端点 |
| `REGION` | 为尚未配置区域的端点提供默认值 |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | 本地路径 |
| `WORKERS` `MULTIPART_CONCURRENCY` | 并发（整数 ≥ 1） |
| `RATE_LIMIT` | 源读取合计上限（`20M`、`20MiB`、`0` 等） |

## 命令

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| 命令 | 作用 | 是否写目标 | 并发与限额 |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | 列出源上全部桶 | 否 | 无 |
| `migration run -c config.yaml` | **推荐。** 先扫描再复制 | 是，但不覆盖 | 扫描单线程。复制使用 `workers`、`multipart_concurrency` 和 `rate_limit` |
| `migration scan -c config.yaml` | 把源清单写入 SQLite | 否 | 单线程。不限速 |
| `migration migrate -c config.yaml` | 只复制 SQLite 里 `pending` 或 `failed` 的行 | 是，但不覆盖 | `workers`、`multipart_concurrency` 和 `rate_limit` |
| `migration status -c config.yaml` | 打印本地计数 | 否 | 无。不访问 S3 |
| `migration diff -c config.yaml` | 列举源端对象，逐个在目标端执行 `HEAD` | 否 | `workers` 控制并行 HEAD。不限速 |
| `migration diff --extra -c config.yaml` | 同上，并列出只在目标上的 key | 否 | `workers` |

所有命令共用的参数：

| 参数 | 含义 |
| --- | --- |
| `-c`、`--config` | 配置文件。默认 `config.yaml` |
| `--bucket NAME` | 只迁一个桶。覆盖 YAML 和 `BUCKET` |
| `--workers N` | `run` / `migrate` 同时复制的对象数，以及 `diff` 的并行 HEAD。必须 ≥ 1 |
| `--multipart-concurrency N` | 单个大对象内部同时上传的分片数。必须 ≥ 1。`list`、`scan`、`status`、`diff` 不使用 |
| `--rate-limit SIZE` | `run` / `migrate` 读取对象正文的合计上限。`0` 表示不限速 |
| `--version` | 打印版本 |
| `--help` | 中英双语帮助 |

`diff` 另外接受 `--extra`。

`status` 打印 `BUCKET`、`TOTAL`、`PENDING`、`DONE`、`CONFLICT`、`SKIPPED`、`FAILED`。没有指定桶时，它读取 `db_path` 旁边的 `migration-*.db`；配置的那个库文件如果已经绑定了桶，也会一起读。还没有任何库时会说明这一点并退出 0。先跑 `scan` 或 `run`。

## 推荐流程

### 迁移全部桶

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

对每个桶，`run` 会：

1. 对来源做 `HeadBucket`。
2. 对来源做 `ListObjectsV2`，写入 SQLite。这一步是单线程。
3. 目标上没有该桶就创建。已有的桶不动。
4. 最多同时复制 `workers` 个对象。如果业务恰好在目标检查后创建同名对象，条件写入会保护该对象。工具会重新核对目标，将结果记为 `done` 或 `conflict`。
5. 对象大小达到 `multipart_threshold` 时采用分片上传。每个对象最多并行传输 `multipart_concurrency` 个分片，所有源端读取共享同一带宽上限。
6. 核对目标上的大小。普通上传的对象在 ETag 可比时再比 ETag。见 [对象状态](#resume-and-object-state)。

桶按顺序迁移，所以 `rate_limit` 就是整个进程的上限。

### 只迁一个桶

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### 源上后来写入的对象

`ListObjectsV2` 不能在服务端按修改时间过滤。要补齐新增对象，需要重新列举整个源桶。新发现的对象会标记为 `pending`。再次被列出的 `failed` 或 `skipped` 行回到 `pending`。`done` 和 `conflict` 保持不变，所以第二遍不会重传，也不会覆盖目标。

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

再跑一次 `run` 就是把这两步合成一条命令。

### 先建立清单，再复制

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # 对象计数，不访问 S3
./bin/migration migrate -c config.yaml --workers 16
```

## 并发

用 `workers` 调整同时复制的对象数。大对象还可以通过 `multipart_concurrency` 控制每个对象并行读写的分片数。默认配置为 16 个对象、每个对象 4 个分片，同时发往单个集群的请求最多约 64 个。请根据磁盘、代理和可用内存选择并发数。

扫描必须等到上一页返回续传令牌后才能请求下一页，因此增大 `--workers` 无法加快扫描。`rate_limit` 只限制复制时从源端读取的对象数据，对 `scan`、`list`、`diff` 和 `status` 均不生效。

```text
workers = 4 个对象同时复制
  对象 A（小文件）  ── 1 次 GET + PUT
  对象 B（小文件）  ── 1 次 GET + PUT
  对象 C（大文件）  ── multipart_concurrency 路 Range GET + UploadPart
  对象 D（大文件）  ── 同上
```

每个正在传输的分片需要在内存中缓存，任务队列最多保留 2,048 个对象描述。HTTP 客户端可复用连接，每个主机最多保留 1,024 条空闲连接。

可按负载选择初始值，再根据实际吞吐量调整。

| 负载 | 建议 |
| --- | --- |
| 大量小对象（KB 到几 MB），链路有余量 | `workers` 16–64，`multipart_concurrency` 保持 4 |
| 大小混合 | `workers` 8–32，`multipart_concurrency` 4–8 |
| 少量超大对象 | `workers` 2–8，`multipart_concurrency` 4–16 |
| 需要排查单个对象的错误 | `--workers 1` |
| 代理返回 503、大量超时，或源盘打满 | 先减 `workers`。慢的是大对象时，再减 `multipart_concurrency` |
| 给生产留带宽 | `--rate-limit 20M`，源看起来正常后再试 `30M` 或 `50M`。`0` 取消限额 |
| 小对象想更快 | 加大 `workers` |
| 单个大对象想更快 | 加大 `multipart_concurrency`，或减小 `multipart_part_size`（字节，最小 5 MiB） |

复制过程中，日志约每 10 秒输出一次进度，包括处理数、完成数、冲突数、跳过数、失败数和字节数。

每个正在复制的小对象在 `spool_dir` 里占一个临时文件，最大约等于 `multipart_threshold`，因为 `PutObject` 需要可回读的请求体。`/tmp` 如果是内存盘，把 `spool_dir` 指到真实磁盘。`workers` 越大，同时存在的临时文件越多。

每份状态库应只供一个迁移进程写入。进程通过一条 SQLite 连接按顺序更新状态，对象数据仍可并行传输。多个进程同时写入同一文件可能出现 `database is locked`。

<a id="rate-limit"></a>

## 限速

所有复制任务及其分片共用 `rate_limit` 设置的源端读取带宽。列举对象、获取响应头和向目标上传不计入这个额度。

小对象会先从源端读到本地磁盘，再上传到目标端。若两步共用网卡，请为上传流量留出带宽。

### 单位

后缀不区分大小写。末尾的 `/s` 可以写，会被忽略。裸数字就是字节每秒。`1.5M` 是 1500000 字节/秒。

| 写法 | 字节/秒 |
| --- | --- |
| `20M`、`20MB` | 20×1000×1000 = 20000000 |
| `30M` | 30000000 |
| `50M`、`50MB` | 50000000 |
| `20Mi`、`20MiB` | 20×1024×1024 = 20971520 |
| `2K`、`2KB` | 2000 |
| `2Ki`、`2KiB` | 2048 |
| `1G`、`1GB` | 1000000000 |
| `1Gi`、`1GiB` | 1073741824 |
| `1T`、`1TB` / `1Ti`、`1TiB` | 1000⁴ / 1024⁴ |
| `1048576` | 1048576 |
| `0`、`0M` 或省略 | 不限速 |

`M` 是十进制。`Mi` 才是 1024 进制。`20M` 大约比 `20MiB` 少 5%（20000000 对 20971520）。负数或无法识别的后缀会在启动时失败。

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # 明确不限速
```

<a id="resume-and-object-state"></a>

## 断点续传与对象状态

第一次 Ctrl-C 或 `SIGTERM` 取消 worker 池。还没开始的对象保持 `pending`。进行中的分片上传会被中止，只释放这次上传，不删除已提交对象。已经写入 SQLite 的状态会保留。第二次信号立即退出，退出码 130。

若源对象的 ETag 在扫描和复制之间发生变化，本次复制会失败。请先用 `scan` 刷新清单，再用 `migrate` 重试，也可直接重新执行 `run`。

被取消或超时的请求记为 `failed`，即使错误文本里还有 `NotFound` 或 `StatusCode: 404`。分片中止经常带 `NoSuchUpload`。那不是源对象缺失。`skipped` 表示源请求完整结束，并且对象确实已经不在。

```bash
./bin/migration migrate -c config.yaml
```

`migrate` 只重试 `pending` 和 `failed`。

| 状态 | 含义 | 再次扫描 |
| --- | --- | --- |
| `pending` | 待复制 | 刷新 size 和 ETag |
| `failed` | 上次失败。`migrate` 会重试。原因在 `last_error` | 回到 `pending` |
| `skipped` | 源请求完整返回 404 / `NoSuchKey` | 该 key 再次被列出时回到 `pending` |
| `done` | 已复制并校验，或目标上已有与清单一致的对象 | 不动 |
| `conflict` | 目标已有该 key，且与清单不一致。未覆盖 | 不动 |

目标端已有同名对象时，只有大小相同，且两端 ETag 均非空、完全相同，才会记为 `done`。其余情况均记为 `conflict`，保留目标对象。分片边界不同，即便内容相同也可能产生不同的 ETag。若在上传成功后、状态落盘前中断，恢复时请人工核实这类冲突。

每条冲突在冲突日志里占一行 TSV，时间是 RFC3339 UTC：

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

键名若包含控制字符或以双引号开头，日志会按 Go 字符串字面量的规则加引号并转义。

只配置了一个桶时，使用写下的 `db_path` 和 `conflict_log`。全桶模式在旁边生成：

- `migration-<桶名>.db`
- `conflict-<桶名>.log`

执行 `migrate` 前须先用 `scan` 或 `run` 建立清单。状态库会绑定桶名和两端地址。使用旧版本的状态库前，请先核实它对应的集群。升级后的首次扫描会记录源地址，首次迁移会记录目标地址。扫描失败或中断后，请先重新执行 `scan` 或 `run`，再使用 `migrate`。旧版本的状态库没有扫描完成标记。如果无法确认清单完整，请重新扫描。

## 用 diff 核对目标

`diff` 通过大小和可比较的 ETag 核对对象，不下载正文。`ok` 中仅校验了大小的对象另计入 `size_only`。添加 `--extra` 会建立临时索引，列出仅存在于目标端的对象。源桶为空时，`diff` 仍会检查目标桶是否存在。目标桶缺失会返回退出码 1。

```bash
./bin/migration diff -c config.yaml --workers 32
./bin/migration diff -c config.yaml --extra
```

```text
=== diff photos ===
ok=1000  missing=2  size_mismatch=0  etag_mismatch=1  extra_on_target=0  size_only=3
一致=1000  缺失=2  大小不一致=0  ETag不一致=1  仅目标有=0  仅比大小=3
  missing  a.txt  not on target
  etag     b.txt  src=abc dst=def
```

第二行汇总是中文，因为命令本身就会打印这一行。最多打印 50 条问题，其余记在末尾的 “and N more” 里。

| 结果 | 含义 |
| --- | --- |
| `missing` | 源端存在，目标端缺失 |
| `size` / `etag` | 同名但内容对不上。常见原因是目标上已有新写入。本工具不会覆盖 |
| `extra` | 只在目标上。需要 `--extra` 才会列出。通常是已经写到新集群的数据。不算失败 |

任一 key 缺失，或大小、ETag 不一致，退出码为 1。`extra_on_target` 不会让命令失败。多个桶时，有缺口的那个桶记为失败，其余桶继续，最后只要有桶失败就退出 1。

## 安全保证

| 保证 | 做法 |
| --- | --- |
| 不覆盖目标对象 | `HeadObject` → `PutObject` / `CompleteMultipartUpload` (`If-None-Match: *`) |
| 不删除 | 没有 `DeleteObject`。分片失败只中止这次上传 |
| 校验桶名 | `HeadBucket`，每一页核对桶名，SQLite 文件里记下桶名 |
| 迁移中源被删除 | 完整返回的源 404 记 `skipped`。取消和超时记 `failed`，下次会重试 |
| 中断后恢复 | 每个对象的状态在 SQLite WAL 里。第一次信号取消 worker；下次 `migrate` 继续 |
| 内存有上限 | 小对象落盘。大对象按分片读。任务队列有上限 |
| 复制后校验 | 总是比大小。普通上传的对象在两边 ETag 可比时再比 ETag |

不做：版本历史、删除标记、Object Lock、桶策略、生命周期规则、双向同步，以及服务端 `CopyObject`（两个集群是分开的，字节经过本进程）。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| `read config … copy config.example.yaml first` | `-c` 指向的文件不存在。先复制 `config.example.yaml` |
| `old.endpoint is required` | 设置 `old.endpoint` 或 `OLD_ENDPOINT`。目标侧同样会报 `new.endpoint is required`。密钥会报 `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`、`WORKERS` 或 YAML 的 `workers` 小于 1。YAML 里的 `0` 表示「默认 16」。命令行或环境变量里的 `0` 是错误 |
| `multipart_concurrency must be >= 1` | 分片并发的同一规则。YAML 里的 `0` 表示「默认 4」 |
| `env WORKERS="…" is not an integer` | `WORKERS` 必须是纯整数。带宽写在 `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | 非末片至少 5 MiB。默认 16 MiB 已经满足。数值不会被自动抬高 |
| `unknown size suffix` 或 `rate limit` | `rate_limit`、`RATE_LIMIT` 或 `--rate-limit` 无法识别。十进制写 `20M`。1024 进制写 `20Mi` 或 `20MiB` |
| `head bucket "…"` | 桶不存在、地址错误，或密钥没有读权限。目标上创建失败会说 `create bucket "…" on target (need s3:CreateBucket)` |
| 设了 `20M` 源仍然很忙 | `20M` 是每秒 20000000 字节的源正文读取。上传不计入这个额度。共用网卡时把数字再调低 |
| `scan` 或 `diff` 没有变慢 | 正常。限额只在 `run` / `migrate` 读对象正文时生效 |
| 加大 `--workers` 后扫描仍然慢 | 正常。`scan` 是单线程。并发从复制阶段开始，`diff` 的 HEAD 也会用到 |
| 大量 503 或超时 | 减小 `workers`。大对象再减小 `multipart_concurrency`，让 `workers × multipart_concurrency` 落在代理 `maxconn` 以内 |
| 大量 `etag mismatch` | 单独核对相关对象的内容。分片方式和服务端加密都可能影响 ETag。 |
| `db_path is bound to bucket "…"` | 这个文件已经属于另一个桶。换一个 `db_path` |
| `database is locked` | 两个进程在写同一个数据库。一个文件只跑一个进程 |
| 磁盘被临时文件写满 | 把 `spool_dir` 放到大磁盘，或减小 `workers` |
| Ctrl-C 退不出来 | 等当前请求结束，或再按一次（退出码 130） |
| `diff` 仍有 `missing` | 若对象状态为 `failed`，先查看 `last_error`，排除原因后重试。若已记为 `done` 或 `conflict`，请另设状态目录，重新建立清单。全桶模式下，仅更换数据库文件名不足以启用新清单。 |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 命令执行完成。冲突以及源端已删除的对象分别计数。 |
| 1 | 配置有误、操作被中断或失败、有对象复制失败，或 `diff` 发现对象缺失或不一致。 |
| 130 | 第二次 `SIGINT` 或 `SIGTERM`。进程立即退出 |

命令错误打印在 stderr，形式是 `error: …`。

## 许可证

Apache License 2.0。Copyright 2024-2026 RustFS, Inc.
