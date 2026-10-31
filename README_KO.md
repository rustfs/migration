# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ZH.md">简体中文</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_DE.md">Deutsch</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ES.md">Español</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_FR.md">français</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_JA.md">日本語</a> |
  한국어 |
  <a href="https://github.com/rustfs/migration/blob/main/README_PT.md">Portuguese</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_RU.md">Русский</a>
</p>

`migration`은 S3 호환 클러스터 사이에서 객체를 한 방향으로 복사합니다. 대상은 직접 운영하는 RustFS와 MinIO, 그리고 S3 API를 말하는 다른 엔드포인트입니다. 요청은 항상 path-style 주소 지정을 사용합니다. 이런 클러스터에서는 그것이 일반적인 방식입니다.

운영 트래픽 옆에서 실행할 수 있습니다. 쓰는 명령은 다음 세 가지를 지킵니다.

- **덮어쓰지 않습니다.** 쓰기 전에 대상에 `HeadObject`를 호출합니다. 일치하면 완료로 기록합니다. 다른 객체는 로그만 남기고 그대로 둡니다.
- **삭제하지 않습니다.** `DeleteObject` 호출이 없습니다. 멀티파트 업로드를 중단하면 그 미완료 업로드만 버리며, 이미 커밋된 객체는 지우지 않습니다.
- **이어서 할 수 있습니다.** 객체마다 로컬 SQLite에 한 행이 있습니다. 멈췄다가 다시 실행해도 끝난 작업은 반복하지 않습니다.

흔한 배치는 옛 클러스터가 이력을 갖고, 새 클러스터가 모든 쓰기를 받으며, 애플리케이션이 이미 양쪽을 읽는 형태입니다. 이 도구는 대상에 아직 없는 키를 채웁니다. 대상의 더 새로운 객체를 원본의 더 오래된 바이트로 바꾸지 않습니다. 같은 키의 충돌은 이 배치에서 예상된 일입니다. `conflict.log`를 읽고 손으로 처리하십시오.

복사되는 것은 현재 객체와 `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, 사용자 메타데이터 `x-amz-meta-*`입니다. 버전 이력, 삭제 마커, Object Lock, 버킷 정책, 수명 주기 규칙은 그 자리에 남습니다.

설정 파일에서 원본은 `old`, 대상은 `new`입니다.

## 문서

| 문서 | 내용 |
| --- | --- |
| 이 파일 | 한국어 설치, 설정, 명령, 조정, 안전 |
| [README.md](README.md) | English, 같은 구성 |
| [README_ZH.md](README_ZH.md) | 简体中文 |
| [README_DE.md](README_DE.md) | Deutsch |
| [README_ES.md](README_ES.md) | Español |
| [README_FR.md](README_FR.md) | français |
| [README_JA.md](README_JA.md) | 日本語 |
| [README_PT.md](README_PT.md) | Portuguese |
| [README_RU.md](README_RU.md) | Русский |
| [docs/USAGE.md](docs/USAGE.md) | 영어 짧은 보충 |
| [docs/使用手册.md](docs/使用手册.md) | 더 긴 중국어 안내 |
| [docs/DESIGN.md](docs/DESIGN.md) | 구조와, 도구가 완화하지 않는 제약 |

`migration --help`와 `migration <command> --help`는 영어와 중국어 도움말을 출력합니다.

## 빠른 시작

Go 1.22 이상. SQLite는 순수 Go이므로 빌드에 CGO가 필요 없습니다.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # 원본과 대상의 엔드포인트와 키
CGO_ENABLED=0 go build -o bin/migration .
# 또는 make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

`bucket:`을 비우면 원본의 모든 버킷을 옮깁니다. `bucket: photos` 또는 `--bucket photos`로 하나를 고정합니다.

`20M`은 십진 메가바이트/초입니다. 20×1000×1000 바이트/초. 공유 네트워크에서 올리기 전에 [속도 제한](#rate-limit)을 읽으십시오.

## 설치

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build`는 같은 명령입니다. 결과는 정적 바이너리 `bin/migration`입니다. `make test`는 `go test ./...`입니다.

`config.yaml`을 보호하십시오(`chmod 600`이 합리적인 기본값입니다). 프로세스는 엔드포인트와 객체 키를 로그합니다. 자격 증명은 로그하지 않습니다.

## 설정

```bash
cp config.example.yaml config.yaml
```

최소한 양쪽의 엔드포인트, 액세스 키, 시크릿 키가 필요합니다. 대상에 버킷이 아직 없으면 대상 키에 `s3:CreateBucket`이 필요합니다. 이미 있는 대상 버킷은 바꾸지 않습니다.

```yaml
old:                              # 원본, 읽기 전용
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # 대상, 읽기 및 쓰기
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # 비움 = 원본의 모든 버킷
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # 작은 객체 임시 디렉터리. 기본은 DB 디렉터리

workers: 16                       # 동시에 다룰 객체 수. 0 또는 생략 = 16
multipart_threshold: 52428800     # 50 MiB 이상은 멀티파트
multipart_part_size: 16777216     # 16 MiB. 5 MiB(5242880) 이상
multipart_concurrency: 4          # 큰 객체 하나의 파트 병렬 수. 0 또는 생략 = 4
rate_limit: 0                     # 원본 읽기 합계 상한. 0 또는 생략 = 무제한
```

`list`와 `scan`은 원본 엔드포인트와 키가 필요합니다. `run`, `migrate`, `diff`는 양쪽이 필요합니다.

### 우선순위

높은 쪽이 이깁니다.

1. CLI: `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`
2. 환경 변수
3. YAML
4. 내장 기본값

| 설정 | 생략 시 기본값 |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB (52428800) |
| `multipart_part_size` | 16 MiB (16777216) |
| `rate_limit` | 무제한 |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | `db_path`의 디렉터리 |
| `region` | `us-east-1` |

예: YAML `workers: 8`, 환경 변수 `WORKERS=32`, CLI `--workers 4`를 함께 쓰면 **4**가 사용됩니다.

### 0의 의미

`0`은 항목마다 뜻이 다릅니다.

| 항목 | YAML `0` 또는 생략 | 환경 변수 `0` | CLI `0` |
| --- | --- | --- | --- |
| `workers` | 기본 16 | 오류 | 오류 (`--workers 0`) |
| `multipart_concurrency` | 기본 4 | 오류 | 오류 |
| `multipart_threshold` | 기본 50 MiB | 환경 변수 없음 | 플래그 없음 |
| `multipart_part_size` | 기본 16 MiB | 환경 변수 없음 | 플래그 없음 |
| `rate_limit` | 무제한 | 무제한 (`RATE_LIMIT=0`) | 무제한 (`--rate-limit 0`) |

음수 병렬 수나 크기는 시작 시 실패합니다. `WORKERS`와 `MULTIPART_CONCURRENCY`는 정수만 가능합니다. 단위는 `RATE_LIMIT`에만 붙입니다. `RATE_LIMIT=20M`은 유효하고 `WORKERS=20M`은 유효하지 않습니다.

`multipart_part_size`는 5 MiB(5242880) 이상이어야 합니다. 더 작은 비최종 파트는 S3가 `EntityTooSmall`로 거부합니다. 더 작은 값은 설정 로드 때 실패합니다. 자동으로 올리지 않습니다. 한 객체가 10000 파트를 넘기면, 업로드가 들어가도록 그 객체의 파트 크기를 키웁니다.

버킷 이름에 공백이나 `/`를 넣을 수 없습니다.

### 환경 변수

설정된 변수가 YAML을 덮어씁니다. 설정되지 않은 변수는 파일을 그대로 둡니다.

| 변수 | 덮어쓰는 대상 |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | 원본 엔드포인트 |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | 대상 엔드포인트 |
| `REGION` | 전용 변수 뒤에도 region이 비어 있는 쪽 |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | 로컬 경로 |
| `WORKERS` `MULTIPART_CONCURRENCY` | 병렬 수 (1 이상의 정수) |
| `RATE_LIMIT` | 원본 읽기 합계 상한 (`20M`, `20MiB`, `0`, …) |

## 명령

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| 명령 | 하는 일 | 대상에 쓰는가 | 병렬과 상한 |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | 원본의 모든 버킷을 나열 | 아니요 | 없음 |
| `migration run -c config.yaml` | **권장.** 목록을 만든 뒤 복사 | 예. 덮어쓰지는 않음 | 목록은 단일 스레드. 복사는 `workers`, `multipart_concurrency`, `rate_limit` |
| `migration scan -c config.yaml` | 원본 목록을 SQLite에 기록 | 아니요 | 단일 스레드. 속도 제한 없음 |
| `migration migrate -c config.yaml` | SQLite의 `pending` 또는 `failed` 행만 복사 | 예. 덮어쓰지는 않음 | `workers`, `multipart_concurrency`, `rate_limit` |
| `migration status -c config.yaml` | 로컬 카운터를 출력 | 아니요 | 없음. S3 호출 없음 |
| `migration diff -c config.yaml` | 원본을 나열하고 대상의 각 키를 `HEAD` | 아니요 | 병렬 HEAD는 `workers`. 속도 제한 없음 |
| `migration diff --extra -c config.yaml` | 같으며, 대상에만 있는 키도 나열 | 아니요 | `workers` |

모든 명령의 공통 플래그:

| 플래그 | 의미 |
| --- | --- |
| `-c`, `--config` | 설정 파일. 기본값 `config.yaml` |
| `--bucket NAME` | 버킷 하나를 고정. YAML과 `BUCKET`을 덮어씀 |
| `--workers N` | `run` / `migrate`에서 동시에 복사하는 객체 수, `diff`의 병렬 HEAD. 1 이상 |
| `--multipart-concurrency N` | 큰 객체 하나 안에서 동시에 올리는 파트 수. 1 이상. `list`, `scan`, `status`, `diff`는 무시 |
| `--rate-limit SIZE` | `run` / `migrate`에서 객체 본문을 읽는 합계 상한. `0`은 무제한 |
| `--version` | 버전 출력 |
| `--help` | 영어와 중국어 도움말 |

`diff`는 `--extra`도 받습니다.

`status`는 `BUCKET`, `TOTAL`, `PENDING`, `DONE`, `CONFLICT`, `SKIPPED`, `FAILED`를 출력합니다. 버킷을 고정하지 않으면 `db_path` 옆의 `migration-*.db`를 읽고, 설정한 데이터베이스가 이미 버킷에 묶여 있으면 그것도 읽습니다. 아직 없으면 그 사실을 알리고 0으로 끝납니다. 먼저 `scan` 또는 `run`을 실행하십시오.

## 절차

### 모든 버킷 이전

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

버킷마다 `run`은 다음을 합니다.

1. 원본에서 `HeadBucket`.
2. 대상에 버킷이 없으면 만듭니다. 이미 있는 버킷은 바꾸지 않습니다.
3. 원본을 `ListObjectsV2`하여 SQLite에 씁니다. 이 단계는 단일 스레드입니다.
4. `workers`개의 복사 고루틴을 시작합니다. 각 객체는 먼저 대상을 `HeadObject`합니다. 없는 키는 원본에서 읽어 대상에 씁니다. 이 원본 읽기는 `rate_limit`을 공유합니다. 목록과 이미 일치하는 키는 `done`이 되고 업로드하지 않습니다. 있지만 일치하지 않는 키는 충돌 로그에 추가하고 덮어쓰지 않습니다.
5. `multipart_threshold` 이상인 객체는 `multipart_concurrency`로 파트를 올립니다. 파트도 같은 바이트 상한을 공유합니다.
6. 대상의 크기를 확인합니다. 단일 파트 객체는 ETag를 비교할 수 있을 때 ETag도 확인합니다. [객체 상태](#resume-and-object-state)를 보십시오.

버킷은 하나씩 복사되므로 `rate_limit`이 프로세스 전체의 상한입니다.

### 버킷 하나

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### 나중에 원본에 쓰인 객체

`ListObjectsV2`는 서버에서 수정 시각으로 거를 수 없습니다. 따라잡기는 다시 하는 전체 목록입니다. 새 키는 `pending`이 됩니다. 다시 나열된 `failed`와 `skipped`는 `pending`으로 돌아갑니다. `done`과 `conflict`는 그대로이므로 두 번째에는 다시 보내지 않고 대상도 덮어쓰지 않습니다.

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

`run`을 다시 실행하는 것은 이 두 단계를 한 명령으로 하는 것입니다.

### 먼저 목록을 만들기

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # 객체 개수. S3 호출 없음
./bin/migration migrate -c config.yaml --workers 16
```

## 병렬

세 조절은 서로 다른 것을 제한합니다.

| 조절 | 제한하는 것 | 올릴 때 | 내릴 때 |
| --- | --- | --- | --- |
| `workers` / `--workers` | 동시에 날고 있는 **객체** 수. `diff`의 HEAD 병렬이기도 함 | 작은 객체가 대부분이고 CPU, 디스크, 프록시에 여유가 있을 때 | 503, 시간 초과, 원본 디스크 포화, 또는 객체 하나를 조사할 때 (`--workers 1`) |
| `multipart_concurrency` / `--multipart-concurrency` | **큰 객체 하나의 파트**를 몇 개 동시에 올리는지 | 객체 하나가 크고 그 흐름이 회선을 더 쓸 수 있을 때 | 큰 객체가 이미 회선을 채우거나, 여러 개가 겹쳐 프록시를 채울 때 |
| `rate_limit` / `--rate-limit` | 그 worker들이 합쳐서 원본에서 초당 몇 바이트를 읽을 수 있는지 | 원본과 네트워크에 여유가 있을 때 | 운영이 원본 클러스터나 같은 NIC를 쓸 때. `20M`부터 시작하고 `30M` 또는 `50M`을 시도 |

`scan`은 `workers`와 `rate_limit`을 쓰지 않습니다. 목록은 한 페이지씩입니다. 각 페이지가 이전 continuation token이 필요하고, 행은 하나의 SQLite 파일에 들어가기 때문입니다. 목록이 느려도 `--workers`로는 빨라지지 않습니다. `list`, `diff`, `status`는 객체 본문을 읽지 않으므로 바이트 상한이 적용되지 않습니다. `diff`의 HEAD에는 `--workers`를 씁니다.

```text
workers = 동시에 4개 객체
  객체 A (작음)  ── GET + PUT 1개
  객체 B (작음)  ── GET + PUT 1개
  객체 C (큼)    ── multipart_concurrency개의 Range GET + UploadPart
  객체 D (큼)    ── 같음
```

한 클러스터로 가는 동시 요청의 정점은 대략 `workers × multipart_concurrency`입니다. 작은 객체는 요청 하나입니다. 멀티파트 임계값에 도달한 객체는 파트 병렬 수를 곱합니다. 기본 `16 × 4`는 64입니다. HAProxy 또는 Nginx의 `maxconn`과 양쪽 클러스터의 디스크는 이 곱을 감당해야 합니다.

HTTP 클라이언트는 호스트당 유휴 연결을 이 곱까지 올립니다(상한 1024). Go 기본값은 호스트당 2개입니다. 이것은 연결 재사용입니다. 진행 중인 작업을 묶는 것은 worker 풀과 파트 세마포어입니다. 대기열은 객체 설명을 최대 2048개까지 두므로, `--workers`를 크게 해도 버킷 전체를 메모리에 올리지 않습니다.

출발점:

| 부하 | 제안 |
| --- | --- |
| 작은 객체가 많고(수 KB에서 수 MB) 회선에 여유가 있음 | `workers` 16–64, `multipart_concurrency`는 4로 유지 |
| 크기가 섞임 | `workers` 8–32, `multipart_concurrency` 4–8 |
| 매우 큰 객체가 적음 | `workers` 2–8, `multipart_concurrency` 4–16 |
| 객체 하나가 실패하고 로그를 읽기 어려움 | `--workers 1` |
| 프록시가 503, 시간 초과, 또는 원본 디스크가 가득 참 | 먼저 `workers`를 낮춥니다. 느린 것이 큰 객체이면 `multipart_concurrency`도 낮춥니다 |
| 운영 대역을 남김 | `--rate-limit 20M`. 원본이 괜찮아 보이면 `30M` 또는 `50M`. `0`은 상한을 제거 |
| 작은 객체를 더 빠르게 | `workers`를 올립니다 |
| 큰 객체 하나를 더 빠르게 | `multipart_concurrency`를 올리거나 `multipart_part_size`를 낮춥니다(바이트, 최소 5 MiB) |

복사 중에는 약 10초마다 진행 줄이 나옵니다. processed, copied, conflicts, skipped, failed, bytes입니다.

날고 있는 작은 객체는 `spool_dir`에 임시 파일을 하나 둡니다. 최대는 대략 `multipart_threshold` 바이트입니다. `PutObject`가 탐색 가능한 본문을 요구하기 때문입니다. `/tmp`가 메모리 디스크이면 `spool_dir`을 실제 디스크로 두십시오. worker가 많을수록 동시에 있는 임시 파일도 늘어납니다.

데이터베이스 파일 하나당 프로세스는 하나입니다. SQLite는 WAL로 열리고 프로세스 안 연결은 하나입니다. 상태 갱신은 직렬이고 객체 바이트는 worker 풀에 남습니다. 같은 파일을 두 번째 프로세스가 쓰면 여전히 `database is locked`가 납니다.

<a id="rate-limit"></a>

## 속도 제한

`rate_limit`은 모든 복사 worker가 공유하는 토큰 버킷 하나입니다. 큰 객체의 파트도 포함합니다. 세는 것은 원본 객체 본문에서 읽은 바이트입니다. List, HEAD, 쓰기 전 충돌 확인은 상한 밖입니다. 읽기가 끝난 뒤의 업로드는 다시 세지 않습니다.

작은 객체는 상한 속도로 원본에서 다 읽은 다음 대상에 씁니다. 원본 읽기는 상한 안에 있습니다. 그 다음 쓰기는 잠깐 더 빠를 수 있습니다. 이전 호스트가 운영과 NIC를 공유하면 그 쓰기도 대역을 씁니다. NIC 전체를 약 40 MB/s 근처에 두고 싶으면 `20M`부터 시작하십시오.

상한은 복사를 느리게 할 뿐입니다. 덮어쓰지 않고 삭제하지도 않습니다. 목록과 이미 일치하는 객체는 계속 건너뜁니다. 첫 Ctrl-C는 토큰을 기다리는 읽기를 취소합니다. 끝나지 않은 객체는 SQLite에 남고 다음 `migrate`에서 이어집니다.

상한은 날고 있는 요청 수를 줄이지 않습니다. 프록시가 503을 주는 것은 여전히 병렬 문제입니다. 먼저 `workers`, 다음으로 `multipart_concurrency`를 낮추십시오.

### 단위

접미사는 대소문자를 가리지 않습니다. 끝의 `/s`는 써도 무시됩니다. 숫자만 쓰면 초당 바이트입니다. `1.5M`은 1,500,000 바이트/초입니다.

| 표기 | 초당 바이트 |
| --- | --- |
| `20M`, `20MB` | 20×1000×1000 = 20000000 |
| `30M` | 30000000 |
| `50M`, `50MB` | 50000000 |
| `20Mi`, `20MiB` | 20×1024×1024 = 20971520 |
| `2K`, `2KB` | 2000 |
| `2Ki`, `2KiB` | 2048 |
| `1G`, `1GB` | 1000000000 |
| `1Gi`, `1GiB` | 1073741824 |
| `1T`, `1TB` / `1Ti`, `1TiB` | 1000⁴ / 1024⁴ |
| `1048576` | 1048576 |
| `0`, `0M`, 또는 생략 | 무제한 |

`M`은 십진입니다. `Mi`가 1024 기준입니다. `20M`은 `20MiB`보다 약 5% 작습니다(20000000 대 20971520). 음수나 알 수 없는 접미사는 시작 시 실패합니다.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # 의도적으로 무제한
```

<a id="resume-and-object-state"></a>

## 재개와 객체 상태

첫 Ctrl-C 또는 `SIGTERM`은 worker 풀을 취소합니다. 아직 시작하지 않은 객체는 `pending`으로 남습니다. 진행 중인 멀티파트는 중단되어 그 업로드만 놓습니다. 커밋된 객체는 삭제하지 않습니다. 이미 SQLite에 쓴 상태는 남습니다. 두 번째 신호는 즉시 종료하고 종료 코드는 130입니다.

대상의 `PutObject`가 성공한 뒤 행이 아직 `done`이 아니면, 객체는 대상에 있는데 행은 `failed`(또는 `pending`)로 남습니다. 다음 `migrate`는 대상을 `HeadObject`합니다. 크기가 일치하고 비교 가능한 ETag도 일치하면 `done`이 됩니다. 크기가 다르거나 비교 가능한 ETag가 다르면 `conflict`가 되고, 그래도 덮어쓰지 않습니다.

취소된 요청, 기한, `request canceled`는 `failed`입니다. 오류 문에 `NotFound`나 `StatusCode: 404`가 있어도 같습니다. 멀티파트 중단은 종종 `NoSuchUpload`를 돌려줍니다. 그것은 원본 객체가 없는 것이 아닙니다. `skipped`는 원본 요청이 끝났고 객체가 정말로 없어졌다는 뜻입니다.

```bash
./bin/migration migrate -c config.yaml
```

`migrate`가 다시 시도하는 것은 `pending`과 `failed`뿐입니다.

| 상태 | 의미 | 이후 스캔 |
| --- | --- | --- |
| `pending` | 복사 대기 | size와 ETag를 갱신 |
| `failed` | 마지막 시도가 실패. `migrate`가 재시도. 이유는 `last_error` | `pending`으로 되돌림 |
| `skipped` | 원본 요청이 404 / `NoSuchKey`로 끝남 | 그 키가 다시 나열되면 `pending`으로 되돌림 |
| `done` | 복사하고 확인했거나, 대상이 이미 목록과 일치 | 그대로 둠 |
| `conflict` | 대상에 키가 있고 목록과 일치하지 않음. 덮어쓰지 않음 | 그대로 둠 |

**일치 규칙.** 크기는 목록의 크기와 같아야 합니다. ETag를 비교하는 것은 양쪽에 ETag가 있고, 어느 쪽도 멀티파트 ETag(`-`를 포함하고 `hash-N` 형태)가 아닐 때뿐입니다. 멀티파트 ETag는 파트 경계에 의존하므로 비교하지 않고 크기만 봅니다. 크기가 다르거나 비교 가능한 ETag가 다르면 충돌입니다.

충돌은 충돌 로그에 TSV 한 줄입니다. 시각은 RFC3339 UTC입니다.

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

버킷을 하나만 설정하면 적은 그대로의 `db_path`와 `conflict_log`를 씁니다. 모든 버킷 모드는 옆에 다음 파일을 만듭니다.

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

데이터베이스 파일은 버킷 하나에 묶입니다. 다른 버킷으로 돌리면 `db_path is bound to bucket "…"`로 실패합니다.

## diff로 대상 확인

`diff`는 원본 버킷을 그 자리에서 나열하고 대상의 각 키를 `HEAD`합니다. 이전용 SQLite 목록은 읽지 않고 업로드도 하지 않습니다.

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

둘째 요약 줄은 중국어입니다. 명령이 그렇게 출력하기 때문입니다. 문제 줄은 최대 50개이고, 나머지는 끝의 “and N more”에 셉니다.

| 결과 | 의미 |
| --- | --- |
| `missing` | 원본에 있고 대상에 없음. `migrate` 또는 `run`을 다시 실행 |
| `size` / `etag` | 같은 키인데 내용이 다름. 대상에 이미 쓰인 경우가 많음. 이 도구는 덮어쓰지 않음 |
| `extra` | 대상에만 있음. `--extra`로 보고. 보통 새 클러스터에 이미 쓰인 데이터. 실패가 아님 |

키가 없거나 크기 또는 ETag가 다르면 종료 코드는 1입니다. `extra_on_target`은 명령을 실패시키지 않습니다. 버킷이 여러 개이면 차이가 있는 버킷을 실패로 두고 나머지는 계속합니다. 하나라도 실패하면 마지막에 1로 끝납니다.

## 안전

| 보장 | 방법 |
| --- | --- |
| 대상 객체를 덮어쓰지 않음 | 쓰기 전 항상 `HeadObject`. 일치는 `done`. 불일치는 `conflict.log`에 쓰고 건너뜀 |
| 삭제하지 않음 | `DeleteObject`가 없음. 실패한 멀티파트는 중단만 함 |
| 버킷을 섞지 않음 | `HeadBucket`, 각 목록 페이지의 버킷 이름 확인, SQLite 파일에 저장한 버킷 이름 |
| 실행 중 원본이 삭제됨 | 끝난 원본 404는 `skipped`. 취소와 시간 초과는 `failed`이며 다시 시도됨 |
| 충돌과 취소 후 재개 | 객체별 상태는 SQLite WAL에 있음. 첫 신호는 worker를 취소. 다음 `migrate`가 이어감 |
| 메모리에 상한이 있음 | 작은 객체는 디스크로. 큰 객체는 파트 단위로 읽음. 작업 대기열에 상한이 있음 |
| 복사 후 확인 | 크기는 항상. 단일 파트이고 양쪽 ETag를 비교할 수 있으면 ETag도 |

범위 밖: 버전 이력, 삭제 마커, Object Lock, 버킷 정책, 수명 주기 규칙, 양방향 동기화, 서버 측 `CopyObject`(클러스터가 분리되어 있어 바이트는 이 프로세스를 통과합니다).

## 문제 해결

| 보이는 것 | 할 일 |
| --- | --- |
| `read config … copy config.example.yaml first` | `-c` 경로가 없음. `config.example.yaml`을 복사 |
| `old.endpoint is required` | `old.endpoint` 또는 `OLD_ENDPOINT`를 설정. 대상 쪽은 같이 `new.endpoint is required`. 키는 `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`, `WORKERS`, 또는 YAML `workers`가 1 미만. YAML의 `0`은 “기본 16”. CLI나 환경 변수의 `0`은 오류 |
| `multipart_concurrency must be >= 1` | 파트 병렬에도 같은 규칙. YAML의 `0`은 “기본 4” |
| `env WORKERS="…" is not an integer` | `WORKERS`는 단위 없는 정수. 대역은 `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | 비최종 파트는 5 MiB 이상. 기본 16 MiB는 이미 유효. 값은 자동으로 오르지 않음 |
| `unknown size suffix` 또는 `rate limit` | `rate_limit`, `RATE_LIMIT`, `--rate-limit`을 해석하지 못함. 십진은 `20M`. 1024 기준은 `20Mi` 또는 `20MiB` |
| `head bucket "…"` | 버킷이 없거나, 엔드포인트가 틀리거나, 키에 읽기 권한이 없음. 대상 생성 실패는 `create bucket "…" on target (need s3:CreateBucket)` |
| `20M`인데도 원본이 바쁨 | `20M`은 원본 본문 읽기가 초당 20000000 바이트. 업로드는 이 상한 밖. NIC를 공유하면 숫자를 낮춤 |
| `scan`이나 `diff`가 느려지지 않음 | 예상된 동작. 상한은 `run` / `migrate`가 객체 본문을 읽을 때만 적용 |
| `--workers`를 올려도 목록이 느림 | 예상된 동작. `scan`은 단일 스레드. 병렬은 복사와 `diff` HEAD부터 |
| 503이나 시간 초과가 많음 | `workers`를 낮춤. 큰 객체이면 `multipart_concurrency`도 낮춰 `workers × multipart_concurrency`가 프록시 `maxconn` 안에 들어가게 함 |
| `etag mismatch`가 많음 | 원본의 SSE나 압축이 ETag를 바꿀 수 있음. `multipart_threshold`를 낮춰 멀티파트로 가거나(크기는 보고 ETag는 비교하지 않음), 손으로 표본 확인 |
| `db_path is bound to bucket "…"` | 그 파일은 이미 다른 버킷에 묶임. 다른 `db_path`를 사용 |
| `database is locked` | 두 프로세스가 같은 데이터베이스를 씀. 파일당 프로세스는 하나 |
| 임시 파일로 디스크가 참 | `spool_dir`을 큰 디스크로 옮기거나 `workers`를 낮춤 |
| Ctrl-C가 돌아오지 않음 | 진행 중인 요청을 기다리거나 한 번 더 누름(종료 코드 130) |
| `diff`가 아직 `missing`을 냄 | `migrate`를 다시 실행하고 `failed` 행의 `last_error`를 읽음 |

## 종료 코드

| 코드 | 의미 |
| --- | --- |
| 0 | 성공. 버킷이 하나일 때 복사 단계의 첫 Ctrl-C(`migrate`, 또는 `run`의 복사 절반)도 0. 끝나지 않은 객체는 SQLite에 남음 |
| 1 | 잘못된 설정, 원격 오류, 버킷 실패, 또는 `diff`가 누락이나 불일치를 찾음. `scan` 중 첫 Ctrl-C, 그리고 버킷이 둘 이상인 실행 중의 중단도 1 |
| 130 | 두 번째 `SIGINT` 또는 `SIGTERM`. 프로세스가 즉시 종료 |

명령 오류는 stderr에 `error: …`로 나옵니다.

## 라이선스

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
