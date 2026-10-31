# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ZH.md">简体中文</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_DE.md">Deutsch</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ES.md">Español</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_FR.md">français</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_JA.md">日本語</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_KO.md">한국어</a> |
  Portuguese |
  <a href="https://github.com/rustfs/migration/blob/main/README_RU.md">Русский</a>
</p>

`migration` copia objetos num só sentido entre dois clusters compatíveis com S3. Destina-se a RustFS e MinIO auto-hospedados, e a qualquer outro endpoint que fale a API S3. Os pedidos usam sempre endereçamento path-style, o habitual nesses clusters.

Pode correr ao lado do tráfego de produção. Três regras valem para cada comando que escreve:

- **Nunca sobrescreve.** Antes de cada escrita, a ferramenta chama `HeadObject` no destino. Uma correspondência fica registada como concluída. Um objeto diferente é apenas registado e deixado intacto.
- **Nunca apaga.** Não há chamada a `DeleteObject`. Abortar um upload multipart descarta só esse upload incompleto, não um objeto já confirmado.
- **Pode retomar.** Cada objeto tem uma linha numa base SQLite local. Parar e voltar a arrancar não repete o trabalho já feito.

Uma disposição comum é um cluster antigo que ainda guarda o histórico, um cluster novo que recebe todas as escritas, e uma aplicação que já lê dos dois. Esta ferramenta preenche as chaves que o destino ainda não tem. Não substitui um objeto mais recente no destino por bytes mais antigos da origem. Conflitos da mesma chave são esperados nessa disposição. Leia `conflict.log` e resolva-os à mão.

A cópia leva o objeto atual, mais `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` e os metadados de utilizador `x-amz-meta-*`. O histórico de versões, os marcadores de eliminação, Object Lock, as políticas de bucket e as regras de ciclo de vida ficam onde estão.

No ficheiro de configuração, a origem é `old` e o destino é `new`.

## Documentação

| Guia | O que é |
| --- | --- |
| Este ficheiro | Instalação, configuração, comandos, afinação e segurança em português |
| [README.md](README.md) | English, a mesma estrutura |
| [README_ZH.md](README_ZH.md) | 简体中文 |
| [README_DE.md](README_DE.md) | Deutsch |
| [README_ES.md](README_ES.md) | Español |
| [README_FR.md](README_FR.md) | français |
| [README_JA.md](README_JA.md) | 日本語 |
| [README_KO.md](README_KO.md) | 한국어 |
| [README_RU.md](README_RU.md) | Русский |
| [docs/USAGE.md](docs/USAGE.md) | Acompanhamento curto em inglês |
| [docs/使用手册.md](docs/使用手册.md) | Manual chinês mais longo |
| [docs/DESIGN.md](docs/DESIGN.md) | Arquitetura e os limites que a ferramenta não relaxa |

`migration --help` e `migration <comando> --help` imprimem a ajuda em inglês e em chinês.

## Início rápido

Go 1.22 ou mais recente. O SQLite é Go puro, por isso a compilação não precisa de CGO.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # endpoints e chaves de origem e destino
CGO_ENABLED=0 go build -o bin/migration .
# ou: make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Um `bucket:` vazio migra cada bucket da origem. `bucket: photos`, ou `--bucket photos`, fixa um só.

`20M` é um megabyte decimal por segundo: 20×1000×1000 bytes/s. Antes de o aumentar numa rede partilhada, leia [Limite de taxa](#rate-limit).

## Instalação

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` corre o mesmo comando. O resultado é o binário estático `bin/migration`. `make test` corre `go test ./...`.

Proteja `config.yaml` (`chmod 600` é um valor razoável). O processo regista endpoints e chaves de objeto. Não regista credenciais.

## Configuração

```bash
cp config.example.yaml config.yaml
```

O mínimo é um endpoint, uma access key e uma secret key de cada lado. A chave de destino precisa de `s3:CreateBucket` quando o bucket ainda não existe. Um bucket de destino que já existe não é modificado.

```yaml
old:                              # origem, só leitura
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # destino, leitura e escrita
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # vazio = cada bucket da origem
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # temporários de objetos pequenos; omissão = diretório da base

workers: 16                       # objetos de cada vez; 0 ou omitido = 16
multipart_threshold: 52428800     # >= 50 MiB usa upload multipart
multipart_part_size: 16777216     # 16 MiB; tem de ser >= 5 MiB (5242880)
multipart_concurrency: 4          # partes de um objeto grande; 0 ou omitido = 4
rate_limit: 0                     # teto agregado de leitura na origem; 0 ou omitido = sem limite
```

`list` e `scan` precisam do endpoint e das chaves da origem. `run`, `migrate` e `diff` precisam dos dois lados.

### Prioridade

A prioridade mais alta ganha:

1. CLI: `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`
2. Variáveis de ambiente
3. YAML
4. Valores predefinidos

| Definição | Predefinição se omitida |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB (52428800) |
| `multipart_part_size` | 16 MiB (16777216) |
| `rate_limit` | sem limite |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | diretório de `db_path` |
| `region` | `us-east-1` |

Exemplo: YAML `workers: 8`, ambiente `WORKERS=32` e `--workers 4` em conjunto usam **4**.

### O que significa zero

`0` não quer dizer o mesmo em cada campo.

| Campo | YAML `0` ou omitido | Ambiente `0` | CLI `0` |
| --- | --- | --- | --- |
| `workers` | predefinição 16 | erro | erro (`--workers 0`) |
| `multipart_concurrency` | predefinição 4 | erro | erro |
| `multipart_threshold` | predefinição 50 MiB | não há variável | não há opção |
| `multipart_part_size` | predefinição 16 MiB | não há variável | não há opção |
| `rate_limit` | sem limite | sem limite (`RATE_LIMIT=0`) | sem limite (`--rate-limit 0`) |

Concorrência ou tamanho negativos falham no arranque. `WORKERS` e `MULTIPART_CONCURRENCY` têm de ser inteiros simples. Sufixos de tamanho só em `RATE_LIMIT`: `RATE_LIMIT=20M` é válido, `WORKERS=20M` não é.

`multipart_part_size` tem de ser pelo menos 5 MiB (5242880). O S3 rejeita uma parte não final mais pequena com `EntityTooSmall`. Um valor menor falha ao carregar a configuração. Não é aumentado em silêncio. Se um objeto precisasse de mais de 10 000 partes, a ferramenta aumenta o tamanho de parte desse objeto para o upload caber.

Um nome de bucket não pode conter espaços nem `/`.

### Variáveis de ambiente

Uma variável definida substitui o YAML. Uma variável ausente deixa o ficheiro como está.

| Variável | Substitui |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | endpoint de origem |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | endpoint de destino |
| `REGION` | cada lado cuja região continua vazia depois das variáveis específicas |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | caminhos locais |
| `WORKERS` `MULTIPART_CONCURRENCY` | concorrência (inteiro ≥ 1) |
| `RATE_LIMIT` | teto agregado de leitura na origem (`20M`, `20MiB`, `0`, …) |

## Comandos

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| Comando | O que faz | Escreve no destino | Concorrência e teto |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | Lista todos os buckets da origem | Não | Nenhuma |
| `migration run -c config.yaml` | **Recomendado.** Inventariar e depois copiar | Sim, sem sobrescrever | O inventário é de uma só thread. A cópia usa `workers`, `multipart_concurrency` e `rate_limit` |
| `migration scan -c config.yaml` | Inventário da origem para SQLite | Não | Uma só thread. Sem limite de taxa |
| `migration migrate -c config.yaml` | Copia só as linhas SQLite em `pending` ou `failed` | Sim, sem sobrescrever | `workers`, `multipart_concurrency` e `rate_limit` |
| `migration status -c config.yaml` | Imprime os contadores locais | Não | Nenhuma. Sem chamadas S3 |
| `migration diff -c config.yaml` | Lista a origem e faz `HEAD` de cada chave no destino | Não | `workers` para HEAD em paralelo. Sem limite de taxa |
| `migration diff --extra -c config.yaml` | O mesmo, e lista chaves que só existem no destino | Não | `workers` |

Opções comuns a todos os comandos:

| Opção | Significado |
| --- | --- |
| `-c`, `--config` | Ficheiro de configuração. Predefinição `config.yaml` |
| `--bucket NAME` | Fixa um bucket. Substitui o YAML e `BUCKET` |
| `--workers N` | Objetos copiados de cada vez em `run` / `migrate`, e HEAD em paralelo em `diff`. Tem de ser ≥ 1 |
| `--multipart-concurrency N` | Partes enviadas de cada vez dentro de um objeto grande. Tem de ser ≥ 1. Ignorado por `list`, `scan`, `status` e `diff` |
| `--rate-limit SIZE` | Teto agregado de leitura de corpos de objeto em `run` / `migrate`. `0` é sem limite |
| `--version` | Imprime a versão |
| `--help` | Ajuda bilingue |

`diff` também aceita `--extra`.

`status` imprime `BUCKET`, `TOTAL`, `PENDING`, `DONE`, `CONFLICT`, `SKIPPED` e `FAILED`. Sem um bucket fixo, lê `migration-*.db` ao lado de `db_path`, e a base configurada se esse ficheiro já estiver ligado a um bucket. Se ainda não houver nada, diz isso e sai com 0. Corra primeiro `scan` ou `run`.

## Fluxos

### Migrar todos os buckets

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Para cada bucket, `run` faz o seguinte:

1. `HeadBucket` na origem.
2. Cria o bucket no destino se faltar. Um bucket existente não é modificado.
3. `ListObjectsV2` da origem para SQLite. Este passo é de uma só thread.
4. Arranca `workers` goroutines de cópia. Cada objeto é primeiro verificado com `HeadObject` no destino. Uma chave em falta é lida da origem e escrita no destino. Essas leituras partilham `rate_limit`. Uma chave que já corresponde ao inventário fica `done` e não é enviada. Uma chave presente que não corresponde é acrescentada ao registo de conflitos e não é sobrescrita.
5. Um objeto a partir de `multipart_threshold` envia as partes com `multipart_concurrency`. Essas partes partilham o mesmo teto de bytes.
6. Verifica o tamanho no destino. Um objeto de uma só parte também é verificado por ETag quando os ETag são comparáveis. Ver [Estado dos objetos](#resume-and-object-state).

Os buckets são copiados um a seguir ao outro, por isso `rate_limit` é o teto de todo o processo.

### Um só bucket

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### Objetos escritos mais tarde na origem

`ListObjectsV2` não consegue filtrar por data de modificação no servidor. A atualização é outra listagem completa. Chaves novas passam a `pending`. Linhas `failed` ou `skipped` voltam a `pending` quando são listadas de novo. `done` e `conflict` ficam como estão, por isso uma segunda passagem não as reenvia nem sobrescreve o destino.

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

Correr `run` outra vez são esses dois passos num só comando.

### Inventariar primeiro

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # contagens de objetos, sem chamadas S3
./bin/migration migrate -c config.yaml --workers 16
```

## Concorrência

Três controlos, três tarefas diferentes:

| Controlo | O que limita | Aumente quando | Diminua quando |
| --- | --- | --- | --- |
| `workers` / `--workers` | Quantos **objetos** estão em voo. Também o paralelismo HEAD de `diff` | Sobretudo objetos pequenos, e CPU, disco e proxy ainda têm margem | 503, tempos limite, um disco de origem saturado, ou está a depurar um objeto (`--workers 1`) |
| `multipart_concurrency` / `--multipart-concurrency` | Quantas **partes de um objeto grande** são enviadas ao mesmo tempo | Um só objeto é grande e esse fluxo ainda pode usar a ligação | Objetos grandes já enchem a ligação, ou vários juntos enchem o proxy |
| `rate_limit` / `--rate-limit` | Quantos **bytes de origem por segundo** todos esses workers podem ler, em conjunto | A origem e a rede ainda têm margem | A produção usa o cluster de origem ou a mesma NIC. Comece em `20M` e depois tente `30M` ou `50M` |

`scan` ignora `workers` e `rate_limit`. A listagem vai página a página porque cada página precisa do token de continuação da anterior, e as linhas vão para um só ficheiro SQLite. Um scan lento não se resolve com `--workers`. `list`, `diff` e `status` não leem corpos de objeto, por isso o teto de bytes não lhes aplica. `diff` usa `--workers` para HEAD.

```text
workers = 4 objetos de cada vez
  objeto A (pequeno)  ── um GET + PUT
  objeto B (pequeno)  ── um GET + PUT
  objeto C (grande)   ── multipart_concurrency GET por intervalo + UploadPart
  objeto D (grande)   ── o mesmo
```

O pico de pedidos em voo contra um cluster é cerca de `workers × multipart_concurrency`. Um objeto pequeno usa um pedido. Um objeto no limiar multipart multiplica pela concorrência das partes. O predefinido `16 × 4` é 64. O `maxconn` do HAProxy ou do Nginx, e os discos dos dois clusters, têm de aguentar esse produto.

O cliente HTTP sobe as ligações inativas por host até esse produto (com teto em 1024), em vez das duas ligações inativas por host do predefinido do Go. Isso é reutilização de ligações. O trabalho em voo é limitado pelo conjunto de workers e pelo semáforo das partes. A fila de espera guarda no máximo 2048 descritores de objeto, por isso um `--workers` grande não carrega o bucket inteiro para a memória.

Pontos de partida:

| Carga | Sugestão |
| --- | --- |
| Muitos objetos pequenos (kilobytes a poucos megabytes), a ligação tem margem | `workers` 16–64, deixe `multipart_concurrency` em 4 |
| Tamanhos mistos | `workers` 8–32, `multipart_concurrency` 4–8 |
| Poucos objetos muito grandes | `workers` 2–8, `multipart_concurrency` 4–16 |
| Um objeto falha e o registo é difícil de ler | `--workers 1` |
| O proxy devolve 503, tempos limite, ou o disco de origem está cheio | Baixe primeiro `workers`. Se o lento forem objetos grandes, baixe também `multipart_concurrency` |
| Reservar largura de banda para a produção | `--rate-limit 20M`, depois `30M` ou `50M` quando a origem parecer saudável. `0` retira o teto |
| Mais rápido com objetos pequenos | Aumente `workers` |
| Mais rápido com um objeto grande | Aumente `multipart_concurrency`, ou baixe `multipart_part_size` (bytes, mínimo 5 MiB) |

Durante uma cópia, uma linha de progresso é registada cerca de cada 10 segundos: processed, copied, conflicts, skipped, failed e bytes.

Cada objeto pequeno em voo tem um ficheiro temporário em `spool_dir`, até cerca de `multipart_threshold` bytes, porque `PutObject` precisa de um corpo em que se possa fazer seek. Se `/tmp` for um disco em memória, aponte `spool_dir` para um disco real. Mais workers significa mais desses ficheiros ao mesmo tempo.

Um processo por ficheiro de base de dados. O SQLite abre em modo WAL com uma só ligação dentro do processo, por isso as atualizações de estado ficam em série e os bytes do objeto ficam no conjunto de workers. Um segundo processo no mesmo ficheiro continua a reportar `database is locked`.

<a id="rate-limit"></a>

## Limite de taxa

`rate_limit` é um único balde de tokens partilhado por cada worker de cópia, incluindo as partes de um objeto grande. Conta os bytes lidos dos corpos de objeto da origem. List, HEAD e a verificação de conflito antes de escrever ficam fora do teto. O upload que se segue a uma leitura concluída não é contado segunda vez.

Um objeto pequeno é lido da origem no teto e depois escrito no destino. As leituras da origem ficam dentro do teto. A escrita seguinte pode ser brevemente mais rápida. Se o host de migração partilhar uma NIC com a produção, essa escrita continua a usar largura de banda. Quando o uso total da NIC deva ficar perto de 40 MB/s, comece em `20M`.

O teto só abranda as cópias. Não sobrescreve e não apaga. Um objeto que já corresponde ao inventário continua a ser saltado. O primeiro Ctrl-C cancela uma leitura que espera tokens. O objeto por terminar fica no SQLite para o próximo `migrate`.

O teto não reduz quantos pedidos estão em voo. Um proxy que devolve 503 continua a ser um problema de concorrência: baixe `workers` e depois `multipart_concurrency`.

### Unidades

Os sufixos ignoram maiúsculas. Um `/s` final é aceite e ignorado. Um número nu são bytes por segundo. `1.5M` são 1 500 000 bytes/s.

| Escrito como | Bytes por segundo |
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
| `0`, `0M` ou omitido | sem limite |

`M` é decimal. `Mi` é binário. `20M` fica cerca de 5% abaixo de `20MiB` (20000000 contra 20971520). Um valor negativo ou um sufixo desconhecido falha no arranque.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # sem limite, de propósito
```

<a id="resume-and-object-state"></a>

## Retoma e estado dos objetos

O primeiro Ctrl-C ou `SIGTERM` cancela o conjunto de workers. Objetos que ainda não começaram ficam `pending`. Um upload multipart em curso é abortado, o que só liberta esse upload e não apaga um objeto confirmado. O estado já escrito no SQLite é mantido. Um segundo sinal sai de imediato com o código 130.

Se o `PutObject` no destino teve sucesso e a linha ainda não estava marcada `done`, a linha fica `failed` (ou `pending`) embora o objeto já esteja no destino. O `migrate` seguinte verifica o destino com `HeadObject`. Quando o tamanho corresponde e qualquer ETag comparável corresponde, a linha passa a `done`. Quando o tamanho difere, ou um ETag comparável difere, a linha passa a `conflict`, e o objeto continua sem ser sobrescrito.

Um pedido cancelado, um prazo e `request canceled` são `failed`, mesmo quando o texto do erro também contém `NotFound` ou `StatusCode: 404`. Um aborto multipart costuma reportar `NoSuchUpload`. Isso não é um objeto de origem em falta. `skipped` significa que o pedido à origem terminou e o objeto tinha mesmo desaparecido.

```bash
./bin/migration migrate -c config.yaml
```

`migrate` só repete `pending` e `failed`.

| Estado | Significado | Um scan posterior |
| --- | --- | --- |
| `pending` | À espera da cópia | Atualiza tamanho e ETag |
| `failed` | A última tentativa falhou. `migrate` repete-a. A mensagem está em `last_error` | Devolve a linha a `pending` |
| `skipped` | O pedido à origem terminou com 404 / `NoSuchKey` | Volta a `pending` se a chave for listada de novo |
| `done` | Copiado e verificado, ou o destino já correspondia ao inventário | Inalterado |
| `conflict` | O destino tem a chave e ela não corresponde ao inventário. Não foi sobrescrita | Inalterado |

**Regra de correspondência.** O tamanho tem de ser igual ao tamanho inventariado. Os ETag só são comparados quando os dois lados têm um e nenhum é um ETag multipart (um valor que contém `-`, com a forma `hash-N`). Os ETag multipart dependem dos limites das partes, por isso não são comparados; a verificação é o tamanho. Um tamanho diferente, ou um ETag comparável diferente, é um conflito.

Cada conflito é uma linha TSV no registo, com marcas de tempo RFC3339 UTC:

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

Um só bucket configurado usa `db_path` e `conflict_log` tal como escritos. O modo de todos os buckets deriva ficheiros vizinhos:

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

Um ficheiro de base está ligado a um bucket. Apontá-lo a outro bucket falha com `db_path is bound to bucket "…"`.

## Verificar o destino com diff

`diff` lista o bucket de origem ao vivo e faz `HEAD` de cada chave no destino. Não lê o inventário SQLite da migração e não envia nada.

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

A segunda linha de resumo repete os contadores em chinês, porque é assim que o comando os imprime. São mostradas no máximo 50 linhas de incidência; o resto é contado numa linha final «and N more».

| Resultado | Significado |
| --- | --- |
| `missing` | Na origem, não no destino. Volte a correr `migrate` ou `run` |
| `size` / `etag` | A mesma chave, o conteúdo não corresponde. Muitas vezes uma escrita já feita no destino. Esta ferramenta não a sobrescreve |
| `extra` | Só no destino. Reportado com `--extra`. Em geral uma escrita que já chegou ao cluster novo. Não é uma falha |

O código de saída é 1 quando alguma chave falta ou o tamanho ou o ETag não correspondem. `extra_on_target` não faz o comando falhar. Com vários buckets, uma lacuna falha esse bucket e o comando continua com os restantes, e sai com 1 se algum bucket falhou.

## Segurança

| Garantia | Como |
| --- | --- |
| Não sobrescrever um objeto de destino | `HeadObject` antes de cada escrita. Uma correspondência é `done`. Uma diferença é `conflict.log` e um salto |
| Não apagar | Não há `DeleteObject`. Um upload multipart falhado só é abortado |
| Não misturar buckets | `HeadBucket`, uma verificação do nome do bucket em cada página de listagem, e um nome de bucket guardado no ficheiro SQLite |
| Eliminações na origem durante a execução | Um 404 de origem já terminado é `skipped`. Cancelamento e tempo limite são `failed` e serão repetidos |
| Retomar após falha e cancelamento | O estado de cada objeto está num ficheiro SQLite WAL. O primeiro sinal cancela os workers; o `migrate` seguinte continua |
| Memória limitada | Objetos pequenos vão para disco. Objetos grandes são lidos por parte. A fila de trabalhos tem teto |
| Verificação após a cópia | Sempre o tamanho. Também o ETag num objeto de uma só parte quando ambos os ETag são comparáveis |

Fora de âmbito: histórico de versões, marcadores de eliminação, Object Lock, políticas de bucket, regras de ciclo de vida, sincronização nos dois sentidos, e `CopyObject` no servidor (os clusters são separados, por isso os bytes passam por este processo).

## Resolução de problemas

| O que se vê | O que fazer |
| --- | --- |
| `read config … copy config.example.yaml first` | Falta o caminho de `-c`. Copie `config.example.yaml` |
| `old.endpoint is required` | Defina `old.endpoint` ou `OLD_ENDPOINT`. O lado de destino reporta da mesma forma `new.endpoint is required`. As chaves reportam `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`, `WORKERS` ou o `workers` do YAML é inferior a 1. Um `0` no YAML significa «predefinição 16». Um `0` na CLI ou no ambiente é um erro |
| `multipart_concurrency must be >= 1` | A mesma regra, para a concorrência das partes. Um `0` no YAML significa «predefinição 4» |
| `env WORKERS="…" is not an integer` | `WORKERS` tem de ser um inteiro sem unidades. A largura de banda vai em `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | Partes não finais têm de ter pelo menos 5 MiB. A predefinição de 16 MiB já é válida. O valor não é aumentado sozinho |
| `unknown size suffix` ou `rate limit` | `rate_limit`, `RATE_LIMIT` ou `--rate-limit` não foi reconhecido. O decimal é `20M`. O binário é `20Mi` ou `20MiB` |
| `head bucket "…"` | O bucket falta, o endpoint está errado, ou a chave não consegue ler. Uma falha ao criar no destino diz `create bucket "…" on target (need s3:CreateBucket)` |
| A origem continua ocupada com `20M` | `20M` são 20000000 bytes/s de corpos lidos na origem. O upload fica fora desse teto. Numa NIC partilhada, baixe o número |
| `scan` ou `diff` não abrandaram | Esperado. O teto aplica-se enquanto `run` / `migrate` leem corpos de objeto |
| O scan continua lento depois de aumentar `--workers` | Esperado. `scan` é de uma só thread. A concorrência começa na cópia, e no HEAD de `diff` |
| Muitos 503 ou tempos limite | Baixe `workers`. Com objetos grandes, baixe `multipart_concurrency` para que `workers × multipart_concurrency` caiba no `maxconn` do proxy |
| Muitos resultados `etag mismatch` | SSE ou compressão na origem podem mudar o ETag. Baixe `multipart_threshold` para seguir o caminho multipart (o tamanho é verificado, o ETag não é comparado), ou amostre os objetos à mão |
| `db_path is bound to bucket "…"` | Esse ficheiro já pertence a outro bucket. Use outro `db_path` |
| `database is locked` | Dois processos escrevem a mesma base. Um processo por ficheiro |
| O disco enche-se de temporários | Mova `spool_dir` para um disco grande, ou baixe `workers` |
| Ctrl-C não volta | Espere o pedido em curso, ou carregue outra vez (saída 130) |
| `diff` continua a reportar `missing` | Volte a correr `migrate` e leia `last_error` das linhas `failed` |

## Códigos de saída

| Código | Significado |
| --- | --- |
| 0 | Sucesso. Com um só bucket, o primeiro Ctrl-C durante a cópia (`migrate`, ou a metade de cópia de `run`) também sai com 0. Objetos por terminar ficam no SQLite |
| 1 | Configuração inválida, um erro remoto, um bucket falhado, ou `diff` encontrou uma chave em falta ou diferente. Também o primeiro Ctrl-C durante `scan`, e uma interrupção enquanto há mais de um bucket na execução |
| 130 | Um segundo `SIGINT` ou `SIGTERM`. O processo sai de imediato |

Os erros do comando são impressos em stderr como `error: …`.

## Licença

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
