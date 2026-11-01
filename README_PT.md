# migration

[English](README.md) | [简体中文](README_ZH.md) | [Deutsch](README_DE.md) | [Español](README_ES.md) | [Français](README_FR.md) | [日本語](README_JA.md) | [한국어](README_KO.md) | **Português** | [Русский](README_RU.md)

**Importante: esta tradução está desatualizada.** Siga o [guia atual](docs/USAGE.md) e os [requisitos de segurança](README.md): congele externamente as escritas e eliminações na origem desde a leitura do inventário até à validação e coordene as eliminações no destino. `--source-frozen` não congela nada automaticamente. Verifique as escritas condicionais e não aceite apenas o código de saída 0 como prova de conclusão.

O `migration` copia a versão atual dos objetos entre dois clusters compatíveis com S3. Por exemplo, pode transferir o histórico de um cluster RustFS ou MinIO enquanto a aplicação já escreve no novo cluster. Na configuração, `old` identifica a origem e `new` o destino. Ambos usam endereços S3 com o nome do bucket no caminho.

A ferramenta copia as chaves em falta, regista os conflitos em `conflict.log` e guarda o progresso numa base de dados SQLite local. Nenhum objeto é eliminado. O destino tem de respeitar `If-None-Match: *` nas operações `PutObject` e `CompleteMultipartUpload`. Assim, mesmo que a aplicação crie a chave depois da verificação inicial, o objeto fica protegido.

A cópia preserva o conteúdo atual do objeto, os cabeçalhos `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language` e `Expires` e os metadados de utilizador `x-amz-meta-*`. O histórico de versões, os marcadores de eliminação, o Object Lock, as políticas dos buckets e as regras do ciclo de vida ficam fora do âmbito da migração.

## Documentação

[Referência de comandos em inglês](docs/USAGE.md) · [Manual de operação em chinês](docs/使用手册.md) · [Arquitetura em inglês](docs/DESIGN.md)

## Início rápido

Para compilar a partir do repositório, utilize Go 1.22 ou posterior. Depois de copiar o modelo, preencha os endereços e as credenciais dos dois clusters em `config.yaml`.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
# ou: make build
cp config.example.yaml config.yaml   # endpoints e chaves de origem e destino
```

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Um `bucket:` vazio migra cada bucket da origem. `bucket: photos`, ou `--bucket photos`, fixa um só.

Utilize `--bucket ""` para selecionar todos os buckets da origem mesmo que a configuração YAML ou `BUCKET` especifiquem apenas um.

Com `--rate-limit 20M`, o conjunto dos processos de cópia pode ler até 20 000 000 bytes por segundo na origem. Consulte as restantes unidades em [Limite de débito](#rate-limit).

## Instalação

Abra uma execução bem-sucedida de [Build packages](https://github.com/rustfs/migration/actions/workflows/packages.yml) e escolha a sua plataforma em **Artifacts**. Descomprima o ficheiro transferido, confirme a soma SHA-256 do pacote e extraia o seu conteúdo. Os pacotes Linux (amd64 e arm64) têm a extensão `.tar.gz`. Para Windows (amd64), o pacote `.zip` contém o executável `migration.exe`.

```bash
./migration --version
```

```powershell
.\migration.exe --version
```

Proteja `config.yaml` (`chmod 600` é um valor razoável). O processo regista endpoints e chaves de objeto. Não regista credenciais.

## Configuração

```bash
cp config.example.yaml config.yaml
```

Indique o endereço HTTP ou HTTPS e as credenciais de cada cluster. Utilize os campos próprios para as credenciais. O URL não pode conter credenciais, parâmetros de consulta ou um fragmento. Para criar um bucket no destino é necessária a permissão `s3:CreateBucket`.

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

O valor de `multipart_part_size` tem de estar entre 5 MiB e 5 GiB (16 MiB por predefinição). Objetos com mais de 5 GiB são sempre enviados em partes. A ferramenta aumenta o tamanho de cada parte para respeitar o limite de 10 000 partes por objeto. A transferência falha se forem necessários mais de 5 GiB por parte.

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

## Procedimentos

### Migrar todos os buckets

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Para cada bucket, `run` faz o seguinte:

1. `HeadBucket` na origem.
2. `ListObjectsV2` da origem para SQLite. Este passo é de uma só thread.
3. Cria o bucket no destino se faltar. Um bucket existente não é modificado.
4. Copiam-se até `workers` objetos em simultâneo. Se outra aplicação criar a chave depois do primeiro HEAD, a escrita condicional protege esse objeto. A ferramenta volta a verificar a chave antes de registar `done` ou `conflict`.
5. A partir de `multipart_threshold`, cada objeto é transferido em partes, até `multipart_concurrency` em simultâneo. Todas as leituras partilham o mesmo limite de débito.
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

`workers` controla quantos objetos são copiados ao mesmo tempo. Para objetos grandes, `multipart_concurrency` define ainda quantas partes de cada objeto são transferidas em paralelo. Com os valores predefinidos de 16 objetos e quatro partes, o número de pedidos em simultâneo a cada cluster pode chegar a cerca de 64. Tenha em conta os discos, o proxy e a memória disponível antes de aumentar estes valores.

`scan` ignora `workers` e `rate_limit`. A listagem vai página a página porque cada página precisa do token de continuação da anterior, e as linhas vão para um só ficheiro SQLite. Um scan lento não se resolve com `--workers`. `list`, `diff` e `status` não leem corpos de objeto, por isso o teto de bytes não lhes aplica. `diff` usa `--workers` para HEAD.

```text
workers = 4 objetos de cada vez
  objeto A (pequeno)  ── um GET + PUT
  objeto B (pequeno)  ── um GET + PUT
  objeto C (grande)   ── multipart_concurrency GET por intervalo + UploadPart
  objeto D (grande)   ── o mesmo
```

Cada parte em transferência fica em memória. A fila de trabalho contém, no máximo, 2048 descritores de objetos. O cliente HTTP reutiliza ligações e pode manter até 1024 ligações inativas por servidor.

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

## Limite de débito

`rate_limit` limita o débito total de leitura na origem, contando todas as cópias e partes em curso. A listagem de chaves, a leitura de cabeçalhos e o envio para o destino não entram neste cálculo.

Os objetos pequenos são primeiro gravados no disco local e só depois enviados para o destino. Se as duas operações partilharem a interface de rede, reserve largura de banda para o envio ao definir o limite de leitura.

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

Se a ETag da origem mudar entre o inventário e a transferência, a cópia falha. Atualize o inventário com `scan` e repita `migrate`, ou volte a executar `run`.

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

Se a chave já existir, só fica no estado `done` se o tamanho coincidir e ambas as ETags estiverem preenchidas e forem iguais. Nos restantes casos, regista-se `conflict` e o objeto de destino é preservado. O mesmo conteúdo pode ter ETags diferentes consoante a divisão em partes. Se a execução tiver sido interrompida logo após o envio, verifique estes conflitos individualmente.

Cada conflito é uma linha TSV no registo, com marcas de tempo RFC3339 UTC:

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

As chaves que contêm carateres de controlo ou começam por aspas duplas são escritas entre aspas, com as sequências de escape da linguagem Go.

Um só bucket configurado usa `db_path` e `conflict_log` tal como escritos. O modo de todos os buckets deriva ficheiros vizinhos:

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

O comando `migrate` requer um inventário criado com `scan` ou `run`. A base de dados fica associada ao bucket e aos endereços dos clusters. Antes de reutilizar uma base de uma versão anterior, confirme a que clusters pertence. A primeira análise regista a origem e a primeira migração regista o destino. Se a análise falhar ou for interrompida, execute novamente `scan` ou `run` antes de utilizar `migrate`. As bases de versões anteriores não registam a conclusão da análise. Em caso de dúvida, repita-a.

## Verificar o destino com diff

O `diff` verifica os tamanhos e as ETags comparáveis sem transferir os objetos. `size_only` indica quantos dos objetos incluídos em `ok` foram verificados apenas pelo tamanho. Com `--extra`, é criado um índice temporário para detetar chaves que só existem no destino. Se o bucket de origem estiver vazio, o `diff` verifica se o bucket de destino existe e devolve o código 1 se estiver em falta.

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

A segunda linha de resumo repete os contadores em chinês, porque é assim que o comando os imprime. São mostradas no máximo 50 linhas de incidência; o resto é contado numa linha final «and N more».

| Resultado | Significado |
| --- | --- |
| `missing` | A chave existe na origem e está em falta no destino |
| `size` / `etag` | A mesma chave, o conteúdo não corresponde. Muitas vezes uma escrita já feita no destino. Esta ferramenta não a sobrescreve |
| `extra` | Só no destino. Reportado com `--extra`. Em geral uma escrita que já chegou ao cluster novo. Não é uma falha |

O código de saída é 1 quando alguma chave falta ou o tamanho ou o ETag não correspondem. `extra_on_target` não faz o comando falhar. Com vários buckets, uma lacuna falha esse bucket e o comando continua com os restantes, e sai com 1 se algum bucket falhou.

## Segurança

| Garantia | Como |
| --- | --- |
| Não sobrescrever um objeto de destino | `HeadObject` → `PutObject` / `CompleteMultipartUpload` (`If-None-Match: *`) |
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
| Muitos resultados `etag mismatch` | Verifique o conteúdo do objeto em separado. A divisão em partes e a cifragem no servidor podem alterar as ETags. |
| `db_path is bound to bucket "…"` | Esse ficheiro já pertence a outro bucket. Use outro `db_path` |
| `database is locked` | Dois processos escrevem a mesma base. Um processo por ficheiro |
| O disco enche-se de temporários | Mova `spool_dir` para um disco grande, ou baixe `workers` |
| Ctrl-C não volta | Espere o pedido em curso, ou carregue outra vez (saída 130) |
| `diff` continua a reportar `missing` | Para `failed`, consulte `last_error` antes de repetir a operação. Para `done` ou `conflict`, crie outro inventário num diretório separado. No modo de todos os buckets não basta mudar o nome do ficheiro da base de dados. |

## Códigos de saída

| Código | Significado |
| --- | --- |
| 0 | Operação concluída. Os conflitos e os objetos ignorados são contabilizados separadamente. |
| 1 | Configuração inválida, interrupção, erro de operação ou de cópia, ou chaves em falta ou divergentes detetadas pelo `diff`. |
| 130 | Um segundo `SIGINT` ou `SIGTERM`. O processo sai de imediato |

Os erros do comando são impressos em stderr como `error: …`.

## Licença

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
