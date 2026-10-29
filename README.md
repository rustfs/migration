# migration

Safe one-way object migration between two S3-compatible clusters
(RustFS / MinIO / S3). **Never overwrites. Never deletes. Resumable.**

在两个 S3 兼容集群之间做安全的单向对象迁移。**永不覆盖、永不删除、可断点续传。**

完整中文手册：[docs/使用手册.md](docs/使用手册.md) · Short English notes: [docs/USAGE.md](docs/USAGE.md)

---

## English (short)

```bash
cp config.example.yaml config.yaml   # endpoints and keys
make build                           # bin/migration (CGO_ENABLED=0, Go 1.22+)
make build-linux                     # bin/migration-linux-amd64 and bin/migration-linux-arm64
./bin/migration run  -c config.yaml --workers 16 --rate-limit 20M
./bin/migration diff -c config.yaml
```

Linux x86_64 / arm64 binaries are static (`CGO_ENABLED=0`, pure-Go SQLite) and are not committed. `20M` is 20×1000×1000 bytes/s. See [docs/使用手册.md](docs/使用手册.md) §2 and §9.3.

| Command | What it does |
| --- | --- |
| `list` | List source buckets (read-only) |
| `run` | Scan, then copy. Recommended |
| `scan` | Inventory into SQLite. Single-threaded |
| `migrate` | Copy `pending` / `failed` rows only |
| `status` | Local counters, no S3 calls |
| `diff` | HEAD the target for every source key |

Leave `bucket:` empty to migrate every source bucket. `--bucket NAME` pins one.

Two concurrency knobs, both overridable from the CLI:

| Knob | Flag | Env | Default | Applies to |
| --- | --- | --- | --- | --- |
| Object workers | `--workers` | `WORKERS` | 16 | `run` / `migrate` copy, and `diff` HEAD |
| Parts of one large object | `--multipart-concurrency` | `MULTIPART_CONCURRENCY` | 4 | multipart upload inside `run` / `migrate` |

Priority is **CLI > environment > YAML > default**. `scan` stays single-threaded because `ListObjectsV2` is sequential. Peak in-flight requests per cluster are about `workers × multipart_concurrency`.

One bandwidth cap, shared by every copy worker (including parts of a large object):

| Knob | Flag | Env | Default | Applies to |
| --- | --- | --- | --- | --- |
| Aggregate source read | `--rate-limit` | `RATE_LIMIT` | unlimited (`0` or omit) | object bodies in `run` / `migrate` |

`20M` and `20MB` are **decimal**: 20×1000×1000 bytes/s. `20Mi` and `20MiB` are **binary**: 20×1024×1024 bytes/s. `30M` and `50M` follow the same decimal rule. A bare number is bytes/s. `scan`, `diff`, and `list` do not read object bodies, so the cap does not apply to them. `0` means unlimited at every layer (CLI, env, and YAML). That is different from `workers`, where YAML `0` means the default of 16.

`HeadObject` runs before every write. A matching object (same size and comparable ETag) is `done`; mismatches go to `conflict.log` and are never overwritten. There is no `DeleteObject`. A finished source 404 is `skipped`; cancel and deadline are `failed`. Ctrl-C cancels the pool; run `migrate` again to resume from SQLite.

`run` exiting 0 does not mean every object succeeded. Per-object errors stay `failed` in SQLite. Check `status`, then `diff`. The startup line `rate_limit=20MB/s` confirms a `20M` cap; `unlimited` means the cap is off.

```text
./bin/migration --help
./bin/migration run --help
```

---

## 中文使用说明

### 1. 安装编译

需要 Go 1.22 或更高版本。SQLite 是纯 Go 实现，**不需要 CGO**。

```bash
git clone https://github.com/rustfs/migration.git
cd migration
make build
# 等价于 CGO_ENABLED=0 go build -o bin/migration .
make build-linux
# bin/migration-linux-amd64 与 bin/migration-linux-arm64，均为静态 ELF
```

得到静态二进制。仓库不提交 `bin/`。细节见手册 §2。

### 2. 配置

```bash
cp config.example.yaml config.yaml
```

最少要填来源和目标的地址、Access Key、Secret Key。地址按 **path-style** 访问（自建 RustFS / MinIO 的常见方式）。

```yaml
old:                              # 来源，只读
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # 目标，读 + 写，需要能 CreateBucket
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # 空 = 来源上的每一个桶
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # 小对象临时目录，默认跟 db 同目录

workers: 16                       # 对象并发，默认 16
multipart_threshold: 52428800     # >= 50MiB 走分片
multipart_part_size: 16777216     # 分片大小 16MiB
multipart_concurrency: 4          # 单个大对象的分片并发，默认 4
rate_limit: 0                     # 源读取合计上限。0 = 不限速。20M = 20000000 字节/秒
```

`bucket:` 留空会迁移来源上的**每一个桶**。只迁一个桶时写成 `bucket: photos`，或命令行加 `--bucket photos`。

环境变量覆盖 YAML，适合容器和密钥管理：

`OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION`  
`NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` `REGION`  
`BUCKET` `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR`  
`WORKERS` `MULTIPART_CONCURRENCY` `RATE_LIMIT`

优先级：**命令行 > 环境变量 > YAML > 默认值**。YAML 里把 `workers` 或 `multipart_concurrency` 写成 `0` 或省略，表示用默认值（16 和 4）。写成负数会直接报错。命令行一旦写了 `--workers` / `--multipart-concurrency`，值必须 `>= 1`。

`rate_limit` 是另一回事：省略或写成 `0` 表示**不限速**，命令行 `--rate-limit 0` 和环境变量 `RATE_LIMIT=0` 也是不限速。它限制的是所有复制 worker 从源读取对象字节的合计速度，不是再开一条并发。

### 3. 常用命令

```bash
./bin/migration --help
./bin/migration run --help
```

| 命令 | 作用 | 是否写目标 |
| --- | --- | --- |
| `migration list -c config.yaml` | 列出源集群全部桶 | 否 |
| `migration run -c config.yaml` | **推荐**。先扫描再复制 | 是（但不覆盖） |
| `migration scan -c config.yaml` | 只扫描，写入本地 SQLite | 否 |
| `migration migrate -c config.yaml` | 只复制 SQLite 里 pending/failed | 是 |
| `migration status -c config.yaml` | 看本地进度，不访问 S3 | 否 |
| `migration diff -c config.yaml` | 按源清单对目标做 HEAD | 否 |
| `migration diff --extra -c config.yaml` | 同上，并列出仅目标上有的 key | 否 |

通用参数：

- `-c, --config` 配置文件，默认 `config.yaml`
- `--bucket NAME` 覆盖 YAML 里的桶名
- `--workers N` 对象并发，覆盖配置。`run` / `migrate` 同时复制 N 个对象，`diff` 同时 HEAD N 个对象
- `--multipart-concurrency N` 单个大对象内部的分片并发，覆盖配置
- `--rate-limit SIZE` 所有 worker 从源读取的合计上限。`20M`、`30M`、`50M` 都是十进制 MB/s；`0` 表示不限速
- `--help` 中英双语帮助
- `--version` 版本号

`--rate-limit` 限制的是从源读取的合计字节。在途请求峰值仍大约是 `workers × multipart_concurrency`，代理 `maxconn` 要撑得住这个乘积。详见手册 §9。

目标上已有且与清单一致的对象记为 `done`，不再上传。大小或可比 ETag 不一致才写入 `conflict.log`，并且永不覆盖。

全量迁移：

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

只迁一个桶，并把大文件分片并行度调到 8：

```bash
./bin/migration run -c config.yaml --bucket photos --workers 8 --multipart-concurrency 8
```

生产时段把源读取压在 20MB/s（十进制），避免占满业务带宽：

```bash
./bin/migration run -c config.yaml --workers 16 --rate-limit 20M
# 30M = 30000000 字节/秒，50M = 50000000 字节/秒
# 若要 20×1024×1024，写成 --rate-limit 20MiB
```

源上后来又写入了新对象时，再扫一遍再补拷（S3 List **不能**按修改时间在服务端过滤）：

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml
./bin/migration diff    -c config.yaml
```

也可以直接再跑一次 `run`。已经是 `done` / `conflict` 的对象不会被重传，也不会被覆盖。

### 4. 并发参数调优

两个旋钮不要混用：

| 参数 | 管什么 | 什么时候加大 | 什么时候减小 |
| --- | --- | --- | --- |
| `workers` / `--workers` | 同时处理多少个**对象** | 小文件非常多，CPU、带宽、代理都还有余量 | 出现 503、连接超时、源盘打满，或要排错（用 `1`） |
| `multipart_concurrency` / `--multipart-concurrency` | **一个大对象里面**同时传多少个分片 | 单个对象很大，想把这块磁盘和网络吃满 | 大对象已经把带宽占满，或多个大对象叠在一起把代理打满 |
| `rate_limit` / `--rate-limit` | 所有 worker **合计**每秒从源读多少字节 | 需要再快，且源和网络有余量 | 业务还在用源集群或同一条网卡。生产上从 `20M` 起，再试 `30M`、`50M` |

`scan` **不使用** `workers`，也不吃 `rate_limit`。列目录必须一页接一页拿 continuation token，结果还要写入同一个 SQLite 文件，所以扫描保持单线程。觉得扫描慢，不是 `--workers` 能解决的。`diff` 和 `list` 只做 HEAD / List，不读对象正文，同样不限速。

经验起点：

- 小文件（KB～几 MB）为主：`workers` 16～64，`multipart_concurrency` 保持 4（基本用不上）
- 大小混合：`workers` 8～32，`multipart_concurrency` 4～8
- 少量超大对象：把 `workers` 降到 2～8（每个对象自己已经很占带宽），`multipart_concurrency` 4～16

峰值大约是 `workers × multipart_concurrency` 个打到**同一个集群**的在途请求。`16 × 4 = 64`。HAProxy / Nginx 的 `maxconn`、源盘、目标盘都要撑得住这个乘积。工具会按这个乘积加大 HTTP 空闲连接，避免 Go 默认每个主机只留 2 条空闲连接；这是连接复用，不是第二道硬限制。

带宽和并发是两层。`workers` 决定同时有几个对象在飞，`rate_limit` 决定这些对象加在一起每秒能从源读多少字节。限额是所有 worker、所有分片共用的一个令牌桶。桶按顺序迁移，所以这也是整个进程的上限。

单位：

| 写法 | 每秒字节数 | 说明 |
| --- | --- | --- |
| `20M`、`20MB` | 20×1000×1000 = 20000000 | 十进制 MB，不是 1024 |
| `30M` | 30000000 | 同上 |
| `50M`、`50MB` | 50000000 | 同上 |
| `20Mi`、`20MiB` | 20×1024×1024 = 20971520 | 二进制 MiB |
| `0` 或省略 | 不限速 | 命令行、环境变量、YAML 都一样 |
| `1048576` | 1048576 | 裸数字就是字节/秒 |

`K`/`KB` = 1000，`Ki`/`KiB` = 1024；`G`/`GB` = 1000³，`Gi`/`GiB` = 1024³。大小写不敏感。

计量的是从**源集群读出的对象字节**。小对象先按这个速度读完再上传，源侧读取不会超过该值；读完之后的上传不重复计费，目标侧可能短时更高。迁移机如果和业务共用网卡，写入还会再占一段带宽，额度请留余量（例如希望网卡总占用不超过约 40MB/s，先设 `20M`）。限速只让复制变慢：已存在的对象仍然跳过，不覆盖、不删除。Ctrl-C 会打断正在等待的限额，没传完的对象留在 SQLite，下次 `migrate` 继续。

`workers` 变大时，每个正在复制的小对象会在 `spool_dir` 里占一个临时文件（最大约等于 `multipart_threshold`）。`/tmp` 如果是内存盘，把 `spool_dir` 指到真实磁盘。

同一个 `db_path` 不要开两个进程一起写。SQLite 使用 WAL，进程内用一条连接串行更新状态，对象数据本身仍由 worker 并行搬运。

### 5. 断点续传

第一次 `Ctrl-C`（或 `SIGTERM`）会取消 worker：没开始的对象留在 SQLite 里，进行中的分片会 `AbortMultipartUpload`（只放弃这次未完成的上传，**不会删除**已提交的对象）。再按一次强制退出，退出码 `130`。

```bash
./bin/migration migrate -c config.yaml
```

`migrate` 只重试 `pending` 和 `failed`。已经 `done` 或 `conflict` 的行不会重传。

每个对象一行状态：

| 状态 | 含义 | 再扫描会改吗 |
| --- | --- | --- |
| `pending` | 待拷 | 会更新 size/etag |
| `failed` | 上次失败，`migrate` 会重试 | 会回到 pending |
| `skipped` | 源请求完整返回 404。取消或超时是 `failed`，不是 `skipped` | 对象后来又出现时会回到 pending |
| `done` | 已拷到目标并校验，或目标上已有一致对象 | 不会动 |
| `conflict` | 目标已有同名对象，且与清单不一致，未覆盖 | 不会动 |

单桶模式使用你配置的 `db_path` / `conflict_log`。全桶模式自动变成 `migration-<桶名>.db` 和 `conflict-<桶名>.log`。

### 6. diff 校验

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

- **missing**：源有、目标没有。再跑 `migrate` 或 `run`
- **size / etag**：同名但内容对不上。本工具不会覆盖，需要人工处理
- **extra_on_target**（需 `--extra`）：只在目标上，通常是业务已经写到新集群，**不算失败**

有 missing、size 或 etag 时退出码为 `1`。`--workers` 只决定 HEAD 的并行度，diff 不会上传。

### 7. 安全模型

| 保证 | 做法 |
| --- | --- |
| 永不覆盖目标对象 | 写之前强制 `HeadObject`。一致则记 `done`；不一致才记入 `conflict.log` 并跳过 |
| 永不删除 | 没有 `DeleteObject`。分片失败只 `AbortMultipartUpload` |
| 不走错桶 | `HeadBucket`，每页校验桶名，SQLite 绑定桶名 |
| 源中途被删 | 只有完整返回的源 404 记 `skipped`；取消或超时记 `failed` |
| 崩溃可续 | 每个对象的状态写在 SQLite WAL 里 |
| 内存有上限 | 小对象落盘；大对象按分片读 |
| 拷后校验 | 比对 size；单分片对象再比 ETag |

不迁移：版本历史、删除标记、Object Lock、桶策略、生命周期规则。

同名冲突是预期现象。新集群上更新过的对象不会被旧内容盖掉。请看 `conflict.log`，不要改成覆盖模式。

### 8. 常见问题

| 现象 | 处理 |
| --- | --- |
| `old.endpoint is required` | 检查 YAML 或 `OLD_ENDPOINT` |
| `workers must be >= 1` | `--workers`、`WORKERS` 或 YAML 里的 `workers` 小于 1。`0` 只在 YAML 里表示「用默认 16」；命令行和环境变量写 `0` 会报错 |
| `multipart_concurrency must be >= 1` | 同上，针对分片并发 |
| `unknown size suffix` / `rate limit` | `--rate-limit`、`RATE_LIMIT` 或 YAML 的 `rate_limit` 写了无法识别的单位。`20M` 是十进制；1024 进制要写 `20Mi` 或 `20MiB` |
| 设了 `20M` 仍然觉得源很忙 | `20M` = 20000000 字节/秒。上传不重复计入这个额度，共用网卡时把数字再调低 |
| 扫描或 diff 没有变慢 | 正常。限额只作用在 `run` / `migrate` 复制对象正文的时候 |
| `head bucket "…" on source` | 桶不存在、地址错，或密钥没有读权限 |
| `create bucket … on target` | 目标密钥缺少 `s3:CreateBucket` |
| 扫描很慢，加大 `--workers` 没用 | 正常。`scan` 是单线程。并发只作用在复制和 diff |
| 大量 503 / 超时 | 减小 `workers`，大对象场景再减小 `multipart_concurrency` |
| 大量 `etag mismatch` | 源端 SSE 或压缩会改 ETag。可把 `multipart_threshold` 调低改走分片（只校验 size），或人工抽查 |
| `db_path is bound to bucket "X"` | 一个 db 只绑定一个桶，换路径 |
| `database is locked` | 不要对同一个 db 起两个进程 |
| 磁盘被临时文件写满 | 把 `spool_dir` 放到大磁盘，或减小 `workers` |
| Ctrl-C 退不出 | 等当前请求结束，或再按一次（退出码 130） |
| diff 还有 missing | 再 `migration migrate`，并看 `failed` 行的 `last_error` |

退出码：`0` 成功（只迁一个桶时，复制阶段第一次 Ctrl-C 也是 0，没做完的对象留在 SQLite）；`1` 配置错误、远端错误、某桶失败、diff 发现缺口，或在扫描 / 多桶切换时被中断；`130` 第二次信号强制退出。

更细的状态机、路径和排错见 [docs/使用手册.md](docs/使用手册.md)。设计约束见 [docs/DESIGN.md](docs/DESIGN.md)。
