# migration

[English](README.md) | [简体中文](README_ZH.md) | [Deutsch](README_DE.md) | [Español](README_ES.md) | [Français](README_FR.md) | **日本語** | [한국어](README_KO.md) | [Português](README_PT.md) | [Русский](README_RU.md)

`migration` は、S3 互換クラスター間でオブジェクトの現行バージョンをコピーするツールです。たとえば、アプリケーションの書き込み先を新しいクラスターに切り替えた後、旧 RustFS や MinIO クラスターに残ったデータを移行できます。設定では `old` が移行元、`new` が移行先です。どちらもバケット名をパスに含める形式でアクセスします。

移行先にないキーをコピーし、競合は `conflict.log` に、進捗はローカルの SQLite に記録します。オブジェクトを削除する操作はありません。移行先は `PutObject` と `CompleteMultipartUpload` の両方で `If-None-Match: *` に対応している必要があります。これにより、存在確認の直後にアプリケーションが同じキーで書き込んだ場合も、そのオブジェクトを保護できます。

コピー対象はオブジェクトの内容、`Content-Type`、`Cache-Control`、`Content-Disposition`、`Content-Encoding`、`Content-Language`、`Expires`、および `x-amz-meta-*` のユーザーメタデータです。過去のバージョン、削除マーカー、Object Lock、バケットポリシー、ライフサイクルルールは移行の対象外です。

## ドキュメント

[コマンド早見表（英語）](docs/USAGE.md) · [運用ガイド（中国語）](docs/使用手册.md) · [設計資料（英語）](docs/DESIGN.md)

## クイックスタート

ソースからビルドする場合は Go 1.22 以降が必要です。設定例をコピーしたら、`config.yaml` に両クラスターの接続先と認証情報を入力してください。

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
# または make build
cp config.example.yaml config.yaml   # ソースとターゲットのエンドポイントとキー
```

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

`bucket:` を空にすると、ソース上のすべてのバケットを移行します。`bucket: photos` か `--bucket photos` で 1 つに固定します。

YAML または `BUCKET` にバケット名が設定されていても、`--bucket ""` を指定すれば移行元の全バケットを対象にできます。

`--rate-limit 20M` を指定すると、移行元からの読み取り速度は全ワーカーの合計で毎秒 20,000,000 バイトまでになります。単位の詳細は[帯域制限](#rate-limit)を参照してください。

## インストール

[Build packages](https://github.com/rustfs/migration/actions/workflows/packages.yml) で成功した実行を開き、**Artifacts** からお使いの環境に合うファイルをダウンロードします。ダウンロードしたファイルを展開し、同梱の SHA-256 でアーカイブを検証してから解凍してください。Linux（amd64、arm64）は `.tar.gz`、Windows（amd64）は `migration.exe` を含む `.zip` 形式です。

```bash
./migration --version
```

```powershell
.\migration.exe --version
```

`config.yaml` は保護してください（`chmod 600` が妥当な初期値です）。プロセスはエンドポイントとオブジェクトキーをログします。資格情報はログしません。

## 設定

```bash
cp config.example.yaml config.yaml
```

両クラスターの HTTP または HTTPS の接続先と認証情報を設定します。認証情報は専用の項目に入力してください。URL に認証情報、クエリ、フラグメントは指定できません。移行先にバケットを作成する場合は `s3:CreateBucket` 権限も必要です。

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

`multipart_part_size` は 5 MiB から 5 GiB の範囲で指定します（既定値は 16 MiB）。5 GiB を超えるオブジェクトは必ず分割して転送します。必要に応じてパートを大きくして合計を 10,000 個以内に収めます。1 パートが 5 GiB を超える場合はエラーになります。

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

## 用途別の操作手順

### すべてのバケットを移行する

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

バケットごとに、`run` は次を行います。

1. ソースで `HeadBucket`。
2. ソースを `ListObjectsV2` し、SQLite に書く。この段階はシングルスレッド。
3. ターゲットにバケットが無ければ作る。すでにあるバケットは変更しない。
4. 最大 `workers` 個のオブジェクトを並列にコピーします。最初の HEAD の直後に同じキーが作成されても、条件付き書き込みによって保護されます。移行先を再確認して `done` または `conflict` を記録します。
5. オブジェクトのサイズが `multipart_threshold` 以上なら、最大 `multipart_concurrency` パートを並列に転送します。すべての読み取りに同じ帯域制限が適用されます。
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

### 先に一覧を作成する

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # オブジェクト件数。S3 は呼ばない
./bin/migration migrate -c config.yaml --workers 16
```

## 並列数

同時にコピーするオブジェクト数は `workers` で調整します。大きなオブジェクトについては、`multipart_concurrency` でオブジェクトごとの同時転送パート数も指定できます。既定値は 16 オブジェクト、各 4 パートなので、1 クラスターあたりの同時リクエスト数は最大でおよそ 64 です。ディスクやプロキシの処理能力、空きメモリを考慮して設定してください。

`scan` は `workers` も `rate_limit` も使いません。一覧は 1 ページずつです。各ページが前の continuation token を必要とし、行は 1 つの SQLite ファイルへ入るからです。走査が遅くても `--workers` では速くなりません。`list`、`diff`、`status` はオブジェクト本体を読まないので、バイト上限は掛かりません。`diff` の HEAD には `--workers` を使います。

```text
workers = 同時に 4 オブジェクト
  オブジェクト A（小）  ── GET + PUT が 1 本
  オブジェクト B（小）  ── GET + PUT が 1 本
  オブジェクト C（大）  ── multipart_concurrency 本の Range GET + UploadPart
  オブジェクト D（大）  ── 同じ
```

転送中のパートはそれぞれメモリに保持されます。キューに格納されるオブジェクトの情報は最大 2,048 件です。HTTP 接続は再利用され、待機接続数の上限はホストごとに 1,024 です。

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

`rate_limit` は、すべてのワーカーとパートを合わせた移行元の読み取り速度を制限します。キーの一覧取得、ヘッダーの取得、移行先へのアップロードは対象外です。

小さいオブジェクトはいったんローカルディスクに保存してからアップロードします。読み取りとアップロードが同じネットワークインターフェースを使う場合は、アップロードに必要な帯域も考慮して読み取り速度を設定してください。

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

スキャン後、コピーするまでに移行元の ETag が変わると転送は失敗します。`scan` で一覧を更新してから `migrate` を再実行するか、`run` を再実行してください。

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

同じキーがすでにある場合、サイズが等しく、双方の ETag が空でなく一致するときだけ `done` と記録します。それ以外は `conflict` として移行先のオブジェクトを保持します。内容が同じでも、パートの区切り方によって ETag は異なります。アップロード直後に処理が中断した場合は、この種の競合を個別に確認してください。

衝突は衝突ログに TSV で 1 行です。時刻は RFC3339 の UTC です。

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

制御文字を含むキーや二重引用符で始まるキーは、Go の文字列リテラルと同じ形式でエスケープして記録します。

バケットを 1 つだけ設定したときは、書いたとおりの `db_path` と `conflict_log` を使います。全バケットモードは隣に次のファイルを作ります。

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

`migrate` の実行には、`scan` または `run` で作成した一覧が必要です。状態データベースにはバケットと接続先が記録されます。旧版のデータベースを使う場合は対象のクラスターを確認してください。更新後、最初のスキャンで移行元が、最初の移行で移行先が登録されます。 スキャンが中断または失敗した場合は、`migrate` の前に `scan` または `run` を再実行してください。旧版のデータベースにはスキャン完了の記録がないため、一覧が完全か判断できない場合は再スキャンしてください。

## diff でターゲットを確認する

`diff` はオブジェクトをダウンロードせず、サイズと比較可能な ETag を照合します。`size_only` は `ok` のうちサイズだけを確認した件数です。`--extra` を付けると、一時インデックスを使って移行先だけに存在するキーも報告します。 移行元のバケットが空でも、`diff` は移行先にバケットが存在するかを確認します。存在しない場合の終了コードは 1 です。

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

2 行目の集計は中国語です。コマンドがそう出力するためです。問題行は最大 50 件で、残りは末尾の “and N more” に数えられます。

| 結果 | 意味 |
| --- | --- |
| `missing` | 移行元には存在するが、移行先では見つからないキー |
| `size` / `etag` | 同じキーで内容が一致しない。ターゲットへの本番書き込みであることが多い。このツールは上書きしない |
| `extra` | ターゲットにしか無い。`--extra` で出る。通常は新しいクラスタへすでに書かれたデータ。失敗ではない |

キーの欠落、またはサイズか ETag の不一致があると終了コードは 1 です。`extra_on_target` ではコマンドは失敗しません。複数バケットでは、差のあるバケットを失敗とし、残りは続行します。どれか失敗していれば最後に 1 で終了します。

## 安全性

| 保証 | 方法 |
| --- | --- |
| ターゲットのオブジェクトを上書きしない | `HeadObject` → `PutObject` / `CompleteMultipartUpload` (`If-None-Match: *`) |
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
| `etag mismatch` が多い | 対象オブジェクトの内容を個別に照合してください。パートの分割方法やサーバー側の暗号化によって ETag が変わることがあります。 |
| `db_path is bound to bucket "…"` | そのファイルは別のバケットに束縛済み。別の `db_path` を使う |
| `database is locked` | 二つのプロセスが同じデータベースを書いている。ファイルにつきプロセスは 1 つ |
| 一時ファイルでディスクが埋まる | `spool_dir` を大きいディスクへ移すか、`workers` を下げる |
| Ctrl-C で戻らない | 進行中のリクエストを待つか、もう一度押す（終了コード 130） |
| `diff` がまだ `missing` を出す | `failed` の場合は `last_error` を確認して再試行します。`done` または `conflict` の場合は、別のディレクトリに新しい一覧を作成してください。全バケットを対象とする場合、データベースのファイル名を変えるだけでは一覧を切り替えられません。 |

## 終了コード

| コード | 意味 |
| --- | --- |
| 0 | 処理が完了しました。競合とスキップは別途集計されます。 |
| 1 | 設定エラー、処理の中断、操作またはコピーの失敗、あるいは `diff` による欠落・不一致の検出。 |
| 130 | 二度目の `SIGINT` または `SIGTERM`。プロセスはすぐに終了する |

コマンドのエラーは stderr に `error: …` と出ます。

## ライセンス

Apache License 2.0。Copyright 2024-2026 RustFS, Inc.
