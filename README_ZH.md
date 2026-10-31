# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> | 简体中文 |
  <a href="https://github.com/rustfs/migration/blob/main/README_DE.md">Deutsch</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=es">Español</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_FR.md">français</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_JA.md">日本語</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=ko">한국어</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=pt">Portuguese</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=ru">Русский</a>
</p>

`migration` 在两个 S3 兼容集群之间做单向对象复制。面向自建 RustFS、MinIO，以及其它讲 S3 API 的端点。请求一律用 path-style，这是这类集群的常见访问方式。

它可以和在线业务一起跑。凡是会写目标的命令，都遵守三条规则：

- **永不覆盖。** 写入之前先对目标做 `HeadObject`。与清单一致就记为已完成。不一致只写日志，目标上的对象保持原样。
- **永不删除。** 没有 `DeleteObject`。中止分片上传只放弃这次未完成的上传，不会删除已经提交的对象。
- **可断点续传。** 每个对象在本地 SQLite 里有一行。停下再跑，已经完成的工作不会重做。

常见布局是：旧集群还留着历史数据，新集群接收全部写入，应用已经对两边做读。本工具把目标上还没有的 key 补过去，不会用源上更旧的内容盖掉目标上更新的对象。同名冲突在这种布局里是预期现象。看 `conflict.log`，手工处理。

复制的是当前对象，以及 `Content-Type`、`Cache-Control`、`Content-Disposition`、`Content-Encoding`、`Content-Language`、`Expires` 和 `x-amz-meta-*` 用户元数据。版本历史、删除标记、Object Lock、桶策略、生命周期规则都留在原地。

配置文件里，来源是 `old`，目标是 `new`。

## 文档

| 文档 | 内容 |
| --- | --- |
| 本文件 | 中文安装、配置、命令、调参和安全说明 |
| [README.md](README.md) | English，同一结构 |
| [README_DE.md](README_DE.md) | Deutsch |
| [README_FR.md](README_FR.md) | français |
| [README_JA.md](README_JA.md) | 日本語 |
| [docs/USAGE.md](docs/USAGE.md) | 英文短说明 |
| [docs/使用手册.md](docs/使用手册.md) | 更细的中文手册 |
| [docs/DESIGN.md](docs/DESIGN.md) | 架构和不会放松的约束 |

Español、한국어、Portuguese、Русский 从语言栏经 readme-i18n 打开，不在本仓库里。

`migration --help` 和 `migration <command> --help` 打印中英双语帮助。

## 快速开始

需要 Go 1.22 或更高版本。SQLite 是纯 Go 实现，编译不需要 CGO。

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # 填写来源和目标的地址、密钥
CGO_ENABLED=0 go build -o bin/migration .
# 或：make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

`bucket:` 留空会迁移来源上的每一个桶。写成 `bucket: photos`，或加上 `--bucket photos`，就只迁这一个桶。

`20M` 是十进制兆字节每秒：20×1000×1000 字节/秒。在共享网络上调高之前，先看 [限速](#rate-limit)。

## 安装编译

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` 跑的是同一条命令。产物是静态二进制 `bin/migration`。`make test` 即 `go test ./...`。

保护好 `config.yaml`（`chmod 600` 是合理默认）。进程会记录端点和对象 key，不会记录密钥。

## 配置

```bash
cp config.example.yaml config.yaml
```

最少要填两边的地址、Access Key、Secret Key。目标上还没有该桶时，目标密钥需要 `s3:CreateBucket`。已经存在的目标桶不会被修改。

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

bucket: ""                        # 空 = 来源上的每一个桶
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # 小对象临时目录，默认跟 db 同目录

workers: 16                       # 同时处理的对象数；0 或省略 = 16
multipart_threshold: 52428800     # >= 50 MiB 走分片上传
multipart_part_size: 16777216     # 16 MiB；必须 >= 5 MiB（5242880）
multipart_concurrency: 4          # 单个大对象的分片并发；0 或省略 = 4
rate_limit: 0                     # 源读取合计上限；0 或省略 = 不限速
```

`list` 和 `scan` 只需要来源地址和密钥。`run`、`migrate`、`diff` 两边都要。

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

`multipart_part_size` 至少 5 MiB（5242880）。S3 会用 `EntityTooSmall` 拒绝更小的非末片。写得更小会在加载配置时失败，不会被悄悄抬高。单个对象的分片数要超过 10000 时，工具会加大该对象的分片，使上传能够完成。

桶名不能包含空白或 `/`。

### 环境变量

设了的变量覆盖 YAML。没设的变量不动文件里的值。

| 变量 | 覆盖 |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | 来源端点 |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | 目标端点 |
| `REGION` | 在上述专用变量之后仍然为空的那一侧 region |
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
| `migration diff -c config.yaml` | 列出源并对目标每个 key 做 `HEAD` | 否 | `workers` 控制并行 HEAD。不限速 |
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
2. 目标上没有该桶就创建。已有的桶不动。
3. 对来源做 `ListObjectsV2`，写入 SQLite。这一步是单线程。
4. 启动 `workers` 个复制协程。每个对象先对目标做 `HeadObject`。缺失的 key 从来源读出、写到目标。这些源读取共用 `rate_limit`。已经与清单一致的 key 记为 `done`，不再上传。存在但不一致的 key 追加到冲突日志，不覆盖。
5. 大小达到或超过 `multipart_threshold` 的对象，用 `multipart_concurrency` 上传分片。这些分片共用同一个字节上限。
6. 核对目标上的大小。单分片对象在 ETag 可比时再比 ETag。见 [对象状态](#resume-and-object-state)。

桶按顺序迁移，所以 `rate_limit` 就是整个进程的上限。

### 只迁一个桶

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### 源上后来写入的对象

`ListObjectsV2` 不能在服务端按修改时间过滤。补拷就是再列一遍全量清单。新 key 变成 `pending`。再次被列出的 `failed` 或 `skipped` 行回到 `pending`。`done` 和 `conflict` 保持不变，所以第二遍不会重传，也不会覆盖目标。

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

再跑一次 `run` 就是把这两步合成一条命令。

### 先盘点再复制

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # 对象计数，不访问 S3
./bin/migration migrate -c config.yaml --workers 16
```

## 并发

三个旋钮，管的事情不一样：

| 旋钮 | 限制什么 | 何时加大 | 何时减小 |
| --- | --- | --- | --- |
| `workers` / `--workers` | 同时在飞的**对象**数。也是 `diff` 的 HEAD 并行度 | 以小对象为主，CPU、磁盘和代理都还有余量 | 出现 503、超时、源盘打满，或要排查单个对象（`--workers 1`） |
| `multipart_concurrency` / `--multipart-concurrency` | **一个大对象里面**同时上传多少个分片 | 单个对象很大，这条流还能再用一些带宽 | 大对象已经占满链路，或几个大对象叠在一起把代理打满 |
| `rate_limit` / `--rate-limit` | 所有这些 worker **合计**每秒能从源读多少字节 | 源和网络还有余量 | 生产还在用源集群或同一块网卡。从 `20M` 起，再试 `30M` 或 `50M` |

`scan` 不用 `workers`，也不吃 `rate_limit`。列目录必须一页接一页，因为每一页都要上一页的 continuation token，而且行要写进同一个 SQLite 文件。扫描慢，加大 `--workers` 没有用。`list`、`diff`、`status` 不读对象正文，所以字节上限对它们不起作用。`diff` 会用 `--workers` 做 HEAD。

```text
workers = 4 个对象同时复制
  对象 A（小文件）  ── 1 次 GET + PUT
  对象 B（小文件）  ── 1 次 GET + PUT
  对象 C（大文件）  ── multipart_concurrency 路 Range GET + UploadPart
  对象 D（大文件）  ── 同上
```

打到同一个集群的在途请求峰值大约是 `workers × multipart_concurrency`。小对象只占一路。达到分片阈值的对象会按分片并发乘上去。默认 `16 × 4` 是 64。HAProxy 或 Nginx 的 `maxconn`，以及两边的磁盘，都要撑得住这个乘积。

HTTP 客户端会把每个主机的空闲连接数提高到这个乘积（上限 1024），用来替代 Go 默认的每主机 2 条空闲连接。这是连接复用。真正限制在途工作的是 worker 池和分片信号量。等待队列最多 2048 个对象描述，所以把 `--workers` 开得很大也不会把整桶清单堆进内存。

经验起点：

| 负载 | 建议 |
| --- | --- |
| 大量小对象（KB 到几 MB），链路有余量 | `workers` 16–64，`multipart_concurrency` 保持 4 |
| 大小混合 | `workers` 8–32，`multipart_concurrency` 4–8 |
| 少量超大对象 | `workers` 2–8，`multipart_concurrency` 4–16 |
| 单个对象失败，日志不好对 | `--workers 1` |
| 代理返回 503、大量超时，或源盘打满 | 先减 `workers`。慢的是大对象时，再减 `multipart_concurrency` |
| 给生产留带宽 | `--rate-limit 20M`，源看起来正常后再试 `30M` 或 `50M`。`0` 取消限额 |
| 小对象想更快 | 加大 `workers` |
| 单个大对象想更快 | 加大 `multipart_concurrency`，或减小 `multipart_part_size`（字节，最小 5 MiB） |

复制过程中大约每 10 秒打一行进度：processed、copied、conflicts、skipped、failed、bytes。

每个正在复制的小对象在 `spool_dir` 里占一个临时文件，最大约等于 `multipart_threshold`，因为 `PutObject` 需要可 seek 的 body。`/tmp` 如果是内存盘，把 `spool_dir` 指到真实磁盘。`workers` 越大，同时存在的临时文件越多。

一个数据库文件只跑一个进程。SQLite 以 WAL 打开，进程内只用一条连接，所以状态更新是串行的，对象字节仍走 worker 池。第二个进程写同一个文件仍会报 `database is locked`。

<a id="rate-limit"></a>

## 限速

`rate_limit` 是所有复制 worker 共用的一个令牌桶，包括同一个大对象里的分片。它统计的是从源读出的对象正文字节。List、HEAD 和写前冲突检查不计入。读完之后的上传也不再计一次。

小对象按限额从来源读完，再写到目标。源侧读取不超过限额。随后的写入可能短时更快。迁移机如果和业务共用网卡，写入还会再占一段带宽。希望网卡总占用停在大约 40 MB/s 附近时，从 `20M` 开始。

限额只让复制变慢。它不覆盖，也不删除。已经与清单一致的对象仍然跳过。第一次 Ctrl-C 会取消正在等令牌的读取。没传完的对象留在 SQLite 里，下次 `migrate` 继续。

限额不减少在途请求数。代理返回 503 仍然是并发问题：先减 `workers`，再减 `multipart_concurrency`。

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

如果目标上的 `PutObject` 已经成功，行还没写成 `done`，这一行会停在 `failed`（或 `pending`），但对象已经在目标上。下次 `migrate` 会对目标做 `HeadObject`。大小一致且可比 ETag 也一致时记为 `done`。大小不同，或可比 ETag 不同，记为 `conflict`，并且仍然不会覆盖。

取消的请求、截止时间，以及 `request canceled`，都记为 `failed`，即使错误文本里还有 `NotFound` 或 `StatusCode: 404`。分片中止经常带 `NoSuchUpload`。那不是源对象缺失。`skipped` 表示源请求完整结束，并且对象确实已经不在。

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

**一致规则。** 大小必须等于清单里的大小。只有两边都有 ETag，且都不是分片 ETag（含 `-`，形如 `hash-N`）时才比较 ETag。分片 ETag 取决于分片边界，所以不比较，只核对大小。大小不同，或可比 ETag 不同，就是冲突。

每条冲突在冲突日志里占一行 TSV，时间是 RFC3339 UTC：

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

只配置了一个桶时，使用写下的 `db_path` 和 `conflict_log`。全桶模式在旁边生成：

- `migration-<桶名>.db`
- `conflict-<桶名>.log`

一个数据库文件只绑定一个桶。拿去迁另一个桶会失败，错误是 `db_path is bound to bucket "…"`。

## 用 diff 核对目标

`diff` 实时列出源桶，并对目标上的每个 key 做 `HEAD`。它不读迁移用的 SQLite 清单，也不会上传。

```bash
./bin/migration diff -c config.yaml --workers 32
./bin/migration diff -c config.yaml --extra
```

```text
=== diff photos ===
ok=1000  missing=2  size_mismatch=0  etag_mismatch=1  extra_on_target=0
一致=1000  缺失=2  大小不一致=0  ETag不一致=1  仅目标有=0
  missing  a.txt  not on target
  etag     b.txt  src=abc dst=def
```

第二行汇总是中文，因为命令本身就会打印这一行。最多打印 50 条问题，其余记在末尾的 “and N more” 里。

| 结果 | 含义 |
| --- | --- |
| `missing` | 源有、目标没有。再跑 `migrate` 或 `run` |
| `size` / `etag` | 同名但内容对不上。常见原因是目标上已有新写入。本工具不会覆盖 |
| `extra` | 只在目标上。需要 `--extra` 才会列出。通常是已经写到新集群的数据。不算失败 |

任一 key 缺失，或大小、ETag 不一致，退出码为 1。`extra_on_target` 不会让命令失败。多个桶时，有缺口的那个桶记为失败，其余桶继续，最后只要有桶失败就退出 1。

## 安全保证

| 保证 | 做法 |
| --- | --- |
| 不覆盖目标对象 | 每次写入前 `HeadObject`。一致记 `done`。不一致写入 `conflict.log` 并跳过 |
| 不删除 | 没有 `DeleteObject`。分片失败只中止这次上传 |
| 不走错桶 | `HeadBucket`，每一页核对桶名，SQLite 文件里记下桶名 |
| 迁移中源被删除 | 完整返回的源 404 记 `skipped`。取消和超时记 `failed`，下次会重试 |
| 崩溃和取消可续 | 每个对象的状态在 SQLite WAL 里。第一次信号取消 worker；下次 `migrate` 继续 |
| 内存有上限 | 小对象落盘。大对象按分片读。任务队列有上限 |
| 拷后校验 | 总是比大小。单分片对象在两边 ETag 可比时再比 ETag |

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
| 大量 `etag mismatch` | 源端 SSE 或压缩会改变 ETag。把 `multipart_threshold` 调低改走分片（只核对大小，不比 ETag），或人工抽查 |
| `db_path is bound to bucket "…"` | 这个文件已经属于另一个桶。换一个 `db_path` |
| `database is locked` | 两个进程在写同一个数据库。一个文件只跑一个进程 |
| 磁盘被临时文件写满 | 把 `spool_dir` 放到大磁盘，或减小 `workers` |
| Ctrl-C 退不出来 | 等当前请求结束，或再按一次（退出码 130） |
| `diff` 仍有 `missing` | 再跑 `migrate`，并看 `failed` 行的 `last_error` |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功。只迁一个桶时，复制阶段第一次 Ctrl-C（`migrate`，或 `run` 的复制半段）也是 0。没做完的对象留在 SQLite |
| 1 | 配置错误、远端错误、某个桶失败，或 `diff` 发现缺失或不一致。扫描时第一次 Ctrl-C，以及一次跑多个桶时被中断，也是 1 |
| 130 | 第二次 `SIGINT` 或 `SIGTERM`。进程立即退出 |

命令错误打印在 stderr，形式是 `error: …`。

## 许可证

Apache License 2.0。Copyright 2024-2026 RustFS, Inc.
