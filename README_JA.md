# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ZH.md">简体中文</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_DE.md">Deutsch</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=es">Español</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_FR.md">français</a> |
  日本語 |
  <a href="https://readme-i18n.com/rustfs/migration?lang=ko">한국어</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=pt">Portuguese</a> |
  <a href="https://readme-i18n.com/rustfs/migration?lang=ru">Русский</a>
</p>

`migration` は、S3 互換クラスタの間でオブジェクトを一方向にコピーします。対象は自前で運用する RustFS と MinIO、および S3 API を話すその他のエンドポイントです。リクエストは常に path-style です。この種のクラスタではそれが通常の呼び方です。

本番トラフィックの横で動かせます。書き込むコマンドは、次の三つを守ります。

- **上書きしない。** 書く前に、ターゲットへ `HeadObject` します。一致すれば完了として記録します。違うオブジェクトはログに残し、そのままにします。
- **削除しない。** `DeleteObject` はありません。マルチパートアップロードの中止は、その未完了のアップロードだけを捨てます。すでにコミットされたオブジェクトは消しません。
- **再開できる。** オブジェクトごとにローカルの SQLite に 1 行あります。止めてから再実行しても、終わった作業はやり直されません。

よくある配置は、古いクラスタが履歴を持ち、新しいクラスタがすべての書き込みを受け、アプリケーションがすでに両方を読む、という形です。このツールは、ターゲットにまだないキーを埋めます。ターゲット上の新しいオブジェクトを、ソースの古いバイトで置き換えることはありません。同じキーの衝突はこの配置では想定内です。`conflict.log` を読み、手で解決してください。

コピーされるのは現在のオブジェクトと、`Content-Type`、`Cache-Control`、`Content-Disposition`、`Content-Encoding`、`Content-Language`、`Expires`、ユーザーメタデータ `x-amz-meta-*` です。バージョン履歴、削除マーカー、Object Lock、バケットポリシー、ライフサイクルルールはそのまま残ります。

設定ファイルでは、ソースが `old`、ターゲットが `new` です。

## ドキュメント

| 文書 | 内容 |
| --- | --- |
| このファイル | 日本語の導入、設定、コマンド、調整、安全性 |
| [README.md](README.md) | English。同じ構成 |
| [README_ZH.md](README_ZH.md) | 简体中文 |
| [README_DE.md](README_DE.md) | Deutsch |
| [README_FR.md](README_FR.md) | français |
| [docs/USAGE.md](docs/USAGE.md) | 英語の短い補足 |
| [docs/使用手册.md](docs/使用手册.md) | より詳しい中国語の手引き |
| [docs/DESIGN.md](docs/DESIGN.md) | アーキテクチャと、ツールが緩めない制約 |

Español、한국어、Portuguese、Русский は言語バーから readme-i18n で開きます。リポジトリには入っていません。

`migration --help` と `migration <command> --help` は、英語と中国語のヘルプを出します。

## クイックスタート

Go 1.22 以降。SQLite は純 Go なので、ビルドに CGO は要りません。

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # ソースとターゲットのエンドポイントとキー
CGO_ENABLED=0 go build -o bin/migration .
# または make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

`bucket:` を空にすると、ソース上のすべてのバケットを移行します。`bucket: photos` か `--bucket photos` で 1 つに固定します。

`20M` は十進のメガバイト毎秒です。20×1000×1000 バイト/秒。共有ネットワークで上げる前に [レート制限](#rate-limit) を読んでください。

## インストール

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` は同じコマンドです。成果物は静的バイナリ `bin/migration` です。`make test` は `go test ./...` です。

`config.yaml` は保護してください（`chmod 600` が妥当な初期値です）。プロセスはエンドポイントとオブジェクトキーをログします。資格情報はログしません。

## 設定

```bash
cp config.example.yaml config.yaml
```

最低限、両側のエンドポイント、アクセスキー、シークレットキーが必要です。ターゲットにバケットがまだ無いときは、ターゲットのキーに `s3:CreateBucket` が必要です。すでにあるターゲットのバケットは変更しません。

```yaml
old:                              # ソース。読み取り専用
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # ターゲット。読み書き
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # 空 = ソース上のすべてのバケット
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # 小さいオブジェクトの一時ディレクトリ。既定は DB と同じディレクトリ

workers: 16                       # 同時に扱うオブジェクト数。0 または省略 = 16
multipart_threshold: 52428800     # 50 MiB 以上はマルチパート
multipart_part_size: 16777216     # 16 MiB。5 MiB（5242880）以上
multipart_concurrency: 4          # 1 つの大きいオブジェクト内のパート並列数。0 または省略 = 4
rate_limit: 0                     # ソース読み取りの合計上限。0 または省略 = 無制限
```

`list` と `scan` はソースのエンドポイントとキーが必要です。`run`、`migrate`、`diff` は両側が必要です。

### 優先順位

高い方が勝ちます。

1. CLI: `--bucket`、`--workers`、`--multipart-concurrency`、`--rate-limit`
2. 環境変数
3. YAML
4. 組み込みの既定値

| 設定 | 省略したときの既定 |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB（52428800） |
| `multipart_part_size` | 16 MiB（16777216） |
| `rate_limit` | 無制限 |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | `db_path` のディレクトリ |
| `region` | `us-east-1` |

例: YAML の `workers: 8`、環境変数 `WORKERS=32`、CLI の `--workers 4` を重ねると、使われるのは **4** です。

### 0 の意味

`0` は項目によって意味が違います。

| 項目 | YAML の `0` または省略 | 環境変数の `0` | CLI の `0` |
| --- | --- | --- | --- |
| `workers` | 既定の 16 | エラー | エラー（`--workers 0`） |
| `multipart_concurrency` | 既定の 4 | エラー | エラー |
| `multipart_threshold` | 既定の 50 MiB | 環境変数はない | フラグはない |
| `multipart_part_size` | 既定の 16 MiB | 環境変数はない | フラグはない |
| `rate_limit` | 無制限 | 無制限（`RATE_LIMIT=0`） | 無制限（`--rate-limit 0`） |

負の並列数やサイズは起動時に失敗します。`WORKERS` と `MULTIPART_CONCURRENCY` は整数だけです。単位は `RATE_LIMIT` にだけ付けます。`RATE_LIMIT=20M` は有効で、`WORKERS=20M` は無効です。

`multipart_part_size` は 5 MiB（5242880）以上です。それより小さい非最終パートは、S3 が `EntityTooSmall` で拒否します。より小さい値は設定の読み込み時に失敗します。自動では引き上げません。1 オブジェクトが 10000 パートを超えるときは、収まるようにそのオブジェクトのパートサイズを大きくします。

バケット名に空白や `/` は使えません。

### 環境変数

設定された変数が YAML を上書きします。未設定の変数はファイルをそのままにします。

| 変数 | 上書きするもの |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | ソースのエンドポイント |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | ターゲットのエンドポイント |
| `REGION` | 専用の変数のあとでも region が空の側 |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | ローカルのパス |
| `WORKERS` `MULTIPART_CONCURRENCY` | 並列数（1 以上の整数） |
| `RATE_LIMIT` | ソース読み取りの合計上限（`20M`、`20MiB`、`0` など） |

## コマンド

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| コマンド | すること | ターゲットへ書くか | 並列数と上限 |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | ソースのバケットをすべて列挙 | いいえ | なし |
| `migration run -c config.yaml` | **推奨。** 走査してからコピー | はい。上書きはしない | 走査はシングルスレッド。コピーは `workers`、`multipart_concurrency`、`rate_limit` |
| `migration scan -c config.yaml` | ソースの目録を SQLite へ | いいえ | シングルスレッド。レート制限なし |
| `migration migrate -c config.yaml` | SQLite の `pending` または `failed` だけをコピー | はい。上書きはしない | `workers`、`multipart_concurrency`、`rate_limit` |
| `migration status -c config.yaml` | ローカルの件数を表示 | いいえ | なし。S3 は呼ばない |
| `migration diff -c config.yaml` | ソースを列挙し、ターゲットの各キーを `HEAD` | いいえ | 並列 HEAD は `workers`。レート制限なし |
| `migration diff --extra -c config.yaml` | 同上に加え、ターゲットにしかないキーを列挙 | いいえ | `workers` |

すべてのコマンドで共通のフラグ:

| フラグ | 意味 |
| --- | --- |
| `-c`, `--config` | 設定ファイル。既定は `config.yaml` |
| `--bucket NAME` | バケットを 1 つに固定。YAML と `BUCKET` を上書き |
| `--workers N` | `run` / `migrate` で同時にコピーするオブジェクト数、および `diff` の並列 HEAD。1 以上 |
| `--multipart-concurrency N` | 1 つの大きいオブジェクトの中で同時に送るパート数。1 以上。`list`、`scan`、`status`、`diff` は無視 |
| `--rate-limit SIZE` | `run` / `migrate` でオブジェクト本体を読む合計上限。`0` は無制限 |
| `--version` | バージョンを表示 |
| `--help` | 英語と中国語のヘルプ |

`diff` は `--extra` も受け付けます。

`status` は `BUCKET`、`TOTAL`、`PENDING`、`DONE`、`CONFLICT`、`SKIPPED`、`FAILED` を出します。バケットを固定していないときは、`db_path` の隣の `migration-*.db` を読みます。設定したデータベースがすでにバケットに束縛されていれば、それも読みます。まだ何も無いときはその旨を出して 0 で終了します。先に `scan` か `run` を実行してください。

## 手順

### すべてのバケットを移行する

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

バケットごとに、`run` は次を行います。

1. ソースで `HeadBucket`。
2. ターゲットにバケットが無ければ作る。すでにあるバケットは変更しない。
3. ソースを `ListObjectsV2` し、SQLite に書く。この段階はシングルスレッド。
4. `workers` 個のコピーゴルーチンを起動する。各オブジェクトは先にターゲットを `HeadObject` する。無いキーはソースから読み、ターゲットへ書く。このソース読み取りは `rate_limit` を共有する。目録とすでに一致するキーは `done` になり、アップロードしない。存在して一致しないキーは衝突ログへ追記し、上書きしない。
5. `multipart_threshold` 以上のオブジェクトは、`multipart_concurrency` でパートを送る。パートも同じバイト上限を共有する。
6. ターゲット上のサイズを確認する。単一パートのオブジェクトは、ETag が比較できるとき ETag も確認する。[オブジェクトの状態](#resume-and-object-state) を参照。

バケットは一つずつコピーされるので、`rate_limit` がプロセス全体の上限です。

### 1 つのバケット

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### あとからソースに書かれたオブジェクト

`ListObjectsV2` は、サーバー側で更新時刻では絞れません。追いつきは、もう一度の全件列挙です。新しいキーは `pending` になります。再度列挙された `failed` と `skipped` は `pending` に戻ります。`done` と `conflict` はそのままなので、二度目は再送せず、ターゲットも上書きしません。

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

もう一度 `run` するのは、この二段階を 1 コマンドにしたものです。

### 先に棚卸しする

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # オブジェクト件数。S3 は呼ばない
./bin/migration migrate -c config.yaml --workers 16
```

## 並列数

三つのつまみは、別々のものを制限します。

| つまみ | 制限するもの | 上げるとき | 下げるとき |
| --- | --- | --- | --- |
| `workers` / `--workers` | 同時に飛んでいる**オブジェクト**数。`diff` の HEAD 並列数でもある | 小さいオブジェクトが主で、CPU、ディスク、プロキシに余力がある | 503、タイムアウト、ソースディスクの飽和、または 1 オブジェクトの調査（`--workers 1`） |
| `multipart_concurrency` / `--multipart-concurrency` | **1 つの大きいオブジェクトのパート**を何本同時に送るか | 1 オブジェクトが大きく、その流れが回線をまだ使える | 大きいオブジェクトがすでに回線を埋めている、または複数が重なってプロキシを埋めている |
| `rate_limit` / `--rate-limit` | それらの worker が合計で、ソースから毎秒何バイト読めるか | ソースとネットワークに余力がある | 本番がソースクラスタ、または同じ NIC を使っている。`20M` から始め、次に `30M` か `50M` |

`scan` は `workers` も `rate_limit` も使いません。一覧は 1 ページずつです。各ページが前の continuation token を必要とし、行は 1 つの SQLite ファイルへ入るからです。走査が遅くても `--workers` では速くなりません。`list`、`diff`、`status` はオブジェクト本体を読まないので、バイト上限は掛かりません。`diff` の HEAD には `--workers` を使います。

```text
workers = 同時に 4 オブジェクト
  オブジェクト A（小）  ── GET + PUT が 1 本
  オブジェクト B（小）  ── GET + PUT が 1 本
  オブジェクト C（大）  ── multipart_concurrency 本の Range GET + UploadPart
  オブジェクト D（大）  ── 同じ
```

1 クラスタへの同時リクエストのピークは、およそ `workers × multipart_concurrency` です。小さいオブジェクトは 1 リクエストです。マルチパート閾値に達したオブジェクトは、パート並列数を掛けます。既定の `16 × 4` は 64 です。HAProxy または Nginx の `maxconn` と、両クラスタのディスクは、この積を運べる必要があります。

HTTP クライアントは、ホストごとのアイドル接続をこの積まで上げます（上限 1024）。Go の既定はホストあたり 2 本です。これは接続の再利用です。実行中の仕事を縛るのは worker プールとパートのセマフォです。待ち行列はオブジェクト記述を最大 2048 個までなので、大きな `--workers` でもバケット全体をメモリに載せません。

出発点:

| 負荷 | 目安 |
| --- | --- |
| 小さいオブジェクトが多い（数 KB から数 MB）で、回線に余力がある | `workers` 16–64。`multipart_concurrency` は 4 のまま |
| サイズが混在 | `workers` 8–32、`multipart_concurrency` 4–8 |
| 非常に大きいオブジェクトが少数 | `workers` 2–8、`multipart_concurrency` 4–16 |
| 1 オブジェクトが失敗し、ログが追いにくい | `--workers 1` |
| プロキシが 503、タイムアウト、またはソースディスクが満杯 | まず `workers` を下げる。遅いのが大きいオブジェクトなら `multipart_concurrency` も下げる |
| 本番の帯域を残す | `--rate-limit 20M`。ソースが健全そうなら `30M` または `50M`。`0` で上限を外す |
| 小さいオブジェクトを速くする | `workers` を上げる |
| 1 つの大きいオブジェクトを速くする | `multipart_concurrency` を上げるか、`multipart_part_size` を下げる（バイト、最小 5 MiB） |

コピー中は約 10 秒ごとに進捗行が出ます。processed、copied、conflicts、skipped、failed、bytes です。

飛んでいる小さいオブジェクトは、`spool_dir` に一時ファイルを 1 つ持ちます。最大はおよそ `multipart_threshold` バイトです。`PutObject` がシーク可能なボディを要求するためです。`/tmp` がメモリディスクなら、`spool_dir` を実ディスクに向けてください。worker が多いほど、同時の一時ファイルも増えます。

データベースファイル 1 つにつきプロセスは 1 つです。SQLite は WAL で開き、プロセス内の接続は 1 本です。状態更新は直列で、オブジェクトのバイトは worker プールに残ります。同じファイルを二つ目のプロセスが書くと、やはり `database is locked` になります。

<a id="rate-limit"></a>

## レート制限

`rate_limit` は、すべてのコピー worker が共有する 1 つのトークンバケットです。大きいオブジェクトのパートも含みます。数えるのは、ソースのオブジェクト本体から読んだバイトです。List、HEAD、書く前の衝突確認は上限の外です。読み終えたあとのアップロードは、もう一度は数えません。

小さいオブジェクトは上限の速度でソースから読み切り、そのあとターゲットへ書きます。ソース側の読み取りは上限以内です。その後の書き込みは短時間だけ速くなることがあります。移行ホストが本番と NIC を共有していると、その書き込みも帯域を使います。NIC 全体をおよそ 40 MB/s 付近に留めたいときは、`20M` から始めてください。

上限はコピーを遅くするだけです。上書きせず、削除もしません。目録とすでに一致するオブジェクトは、引き続きスキップされます。最初の Ctrl-C は、トークン待ちの読み取りを取り消します。終わっていないオブジェクトは SQLite に残り、次の `migrate` で続きます。

上限は、飛んでいるリクエストの数を減らしません。プロキシが 503 を返すのは、依然として並列数の問題です。先に `workers`、次に `multipart_concurrency` を下げてください。

### 単位

接尾辞の大文字小文字は区別しません。末尾の `/s` は書けますが無視されます。裸の数はバイト毎秒です。`1.5M` は 1,500,000 バイト/秒です。

| 書き方 | バイト毎秒 |
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
| `0`、`0M`、または省略 | 無制限 |

`M` は十進です。`Mi` が 1024 基準です。`20M` は `20MiB` より約 5% 小さい値です（20000000 対 20971520）。負の値や未知の接尾辞は起動時に失敗します。

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # 意図して無制限
```

<a id="resume-and-object-state"></a>

## 再開とオブジェクトの状態

最初の Ctrl-C または `SIGTERM` は worker プールを取り消します。まだ始まっていないオブジェクトは `pending` のままです。進行中のマルチパートは中止され、そのアップロードだけが解放されます。コミット済みオブジェクトは削除されません。すでに SQLite へ書いた状態は残ります。二度目のシグナルは即座に終了し、終了コードは 130 です。

ターゲットの `PutObject` が成功したあと、行がまだ `done` になっていないと、オブジェクトはターゲットにあるのに行は `failed`（または `pending`）のままです。次の `migrate` はターゲットを `HeadObject` します。サイズが一致し、比較できる ETag も一致すれば `done` になります。サイズが違う、または比較できる ETag が違うときは `conflict` になり、それでも上書きしません。

取り消されたリクエスト、デッドライン、`request canceled` は `failed` です。エラー文に `NotFound` や `StatusCode: 404` が含まれていても同じです。マルチパートの中止はしばしば `NoSuchUpload` を返します。それはソースオブジェクトの欠落ではありません。`skipped` は、ソースのリクエストが完了し、オブジェクトが本当に無くなっていたことを意味します。

```bash
./bin/migration migrate -c config.yaml
```

`migrate` が再試行するのは `pending` と `failed` だけです。

| 状態 | 意味 | 次の走査 |
| --- | --- | --- |
| `pending` | コピー待ち | size と ETag を更新する |
| `failed` | 直前の試行が失敗。`migrate` が再試行する。理由は `last_error` | `pending` に戻す |
| `skipped` | ソースのリクエストが 404 / `NoSuchKey` で完了した | そのキーが再び列挙されれば `pending` に戻す |
| `done` | コピーして検証した、またはターゲットがすでに目録と一致していた | 変えない |
| `conflict` | ターゲットにキーがあり、目録と一致しない。上書きしていない | 変えない |

**一致の規則。** サイズは目録のサイズと等しくなければなりません。ETag を比べるのは、両側に ETag があり、どちらもマルチパート ETag（`-` を含み、`hash-N` の形）ではないときだけです。マルチパート ETag はパート境界に依存するので比べず、サイズだけを見ます。サイズが違う、または比較できる ETag が違うときは衝突です。

衝突は衝突ログに TSV で 1 行です。時刻は RFC3339 の UTC です。

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

バケットを 1 つだけ設定したときは、書いたとおりの `db_path` と `conflict_log` を使います。全バケットモードは隣に次のファイルを作ります。

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

データベースファイルは 1 つのバケットに束縛されます。別のバケットに向けると、`db_path is bound to bucket "…"` で失敗します。

## diff でターゲットを確認する

`diff` はソースのバケットをその場で列挙し、ターゲットの各キーを `HEAD` します。移行用の SQLite 目録は読まず、アップロードもしません。

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

2 行目の集計は中国語です。コマンドがそう出力するためです。問題行は最大 50 件で、残りは末尾の “and N more” に数えられます。

| 結果 | 意味 |
| --- | --- |
| `missing` | ソースにあり、ターゲットに無い。`migrate` か `run` を再実行 |
| `size` / `etag` | 同じキーで内容が一致しない。ターゲットへの本番書き込みであることが多い。このツールは上書きしない |
| `extra` | ターゲットにしか無い。`--extra` で出る。通常は新しいクラスタへすでに書かれたデータ。失敗ではない |

キーの欠落、またはサイズか ETag の不一致があると終了コードは 1 です。`extra_on_target` ではコマンドは失敗しません。複数バケットでは、差のあるバケットを失敗とし、残りは続行します。どれか失敗していれば最後に 1 で終了します。

## 安全性

| 保証 | 方法 |
| --- | --- |
| ターゲットのオブジェクトを上書きしない | 書く前に必ず `HeadObject`。一致は `done`。不一致は `conflict.log` に書いてスキップ |
| 削除しない | `DeleteObject` は無い。失敗したマルチパートは中止するだけ |
| バケットを取り違えない | `HeadBucket`、各リストページのバケット名確認、SQLite ファイルに保存したバケット名 |
| 実行中にソースが削除される | 完了したソース 404 は `skipped`。取り消しとタイムアウトは `failed` になり、再試行される |
| クラッシュと取り消しから再開できる | オブジェクトごとの状態は SQLite の WAL にある。最初のシグナルは worker を取り消す。次の `migrate` が続ける |
| メモリに上限がある | 小さいオブジェクトはディスクへ。大きいオブジェクトはパート単位で読む。ジョブ待ち行列には上限がある |
| コピー後に確認する | サイズは常に。単一パートで両側の ETag が比較できるときは ETag も |

対象外: バージョン履歴、削除マーカー、Object Lock、バケットポリシー、ライフサイクルルール、双方向同期、サーバー側 `CopyObject`（クラスタは別なので、バイトはこのプロセスを通ります）。

## トラブルシューティング

| 見えるもの | 対処 |
| --- | --- |
| `read config … copy config.example.yaml first` | `-c` のパスが無い。`config.example.yaml` をコピーする |
| `old.endpoint is required` | `old.endpoint` か `OLD_ENDPOINT` を設定する。ターゲット側は同様に `new.endpoint is required`。キーは `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`、`WORKERS`、または YAML の `workers` が 1 未満。YAML の `0` は「既定の 16」。CLI や環境変数の `0` はエラー |
| `multipart_concurrency must be >= 1` | パート並列数にも同じ規則。YAML の `0` は「既定の 4」 |
| `env WORKERS="…" is not an integer` | `WORKERS` は裸の整数。帯域は `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | 非最終パートは 5 MiB 以上。既定の 16 MiB はすでに有効。値は自動では上がらない |
| `unknown size suffix` または `rate limit` | `rate_limit`、`RATE_LIMIT`、`--rate-limit` が解釈できない。十進は `20M`。1024 基準は `20Mi` または `20MiB` |
| `head bucket "…"` | バケットが無い、エンドポイントが違う、またはキーに読み取り権限が無い。ターゲットでの作成失敗は `create bucket "…" on target (need s3:CreateBucket)` |
| `20M` でもソースが忙しい | `20M` はソース本体の読み取りが毎秒 20000000 バイト。アップロードはこの上限の外。NIC を共有しているなら数値を下げる |
| `scan` や `diff` が遅くならない | 想定どおり。上限が掛かるのは `run` / `migrate` がオブジェクト本体を読むとき |
| `--workers` を上げても走査が遅い | 想定どおり。`scan` はシングルスレッド。並列はコピーから、および `diff` の HEAD から |
| 503 やタイムアウトが多い | `workers` を下げる。大きいオブジェクトなら `multipart_concurrency` も下げ、`workers × multipart_concurrency` をプロキシの `maxconn` に収める |
| `etag mismatch` が多い | ソースの SSE や圧縮が ETag を変えることがある。`multipart_threshold` を下げてマルチパートにする（サイズは見て、ETag は比べない）か、手で抜き取り確認する |
| `db_path is bound to bucket "…"` | そのファイルは別のバケットに束縛済み。別の `db_path` を使う |
| `database is locked` | 二つのプロセスが同じデータベースを書いている。ファイルにつきプロセスは 1 つ |
| 一時ファイルでディスクが埋まる | `spool_dir` を大きいディスクへ移すか、`workers` を下げる |
| Ctrl-C で戻らない | 進行中のリクエストを待つか、もう一度押す（終了コード 130） |
| `diff` がまだ `missing` を出す | `migrate` を再実行し、`failed` 行の `last_error` を読む |

## 終了コード

| コード | 意味 |
| --- | --- |
| 0 | 成功。バケットが 1 つのとき、コピー段階の最初の Ctrl-C（`migrate`、または `run` のコピー側）も 0。終わっていないオブジェクトは SQLite に残る |
| 1 | 設定の誤り、リモートのエラー、バケットの失敗、または `diff` が欠落か不一致を見つけた。`scan` 中の最初の Ctrl-C、および複数バケットの実行中の中断も 1 |
| 130 | 二度目の `SIGINT` または `SIGTERM`。プロセスはすぐに終了する |

コマンドのエラーは stderr に `error: …` と出ます。

## ライセンス

Apache License 2.0。Copyright 2024-2026 RustFS, Inc.
