# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ZH.md">简体中文</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_DE.md">Deutsch</a> |
  Español |
  <a href="https://github.com/rustfs/migration/blob/main/README_FR.md">français</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_JA.md">日本語</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_KO.md">한국어</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_PT.md">Portuguese</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_RU.md">Русский</a>
</p>

`migration` copia objetos en un solo sentido entre dos clústeres compatibles con S3. Está pensado para RustFS y MinIO autoalojados, y para cualquier otro extremo que hable la API S3. Las peticiones usan siempre direccionamiento path-style, que es lo habitual en esos clústeres.

Puede ejecutarse junto al tráfico en producción. Tres reglas valen para cada comando que escribe:

- **Nunca sobrescribe.** Antes de cada escritura, la herramienta llama a `HeadObject` en el destino. Una coincidencia queda registrada como terminada. Un objeto distinto solo se anota y se deja intacto.
- **Nunca elimina.** No hay ninguna llamada a `DeleteObject`. Abortar una carga multipart solo descarta esa carga incompleta, no un objeto ya confirmado.
- **Se puede reanudar.** Cada objeto tiene una fila en una base SQLite local. Parar y volver a arrancar no repite el trabajo ya terminado.

Una disposición habitual es un clúster antiguo que aún guarda el historial, un clúster nuevo que recibe todas las escrituras y una aplicación que ya lee de ambos. Esta herramienta rellena las claves que el destino todavía no tiene. No sustituye un objeto más reciente en el destino por bytes más antiguos de la fuente. Los conflictos de la misma clave son esperables en esa disposición. Lea `conflict.log` y resuélvalos a mano.

La copia lleva el objeto actual, más `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` y los metadatos de usuario `x-amz-meta-*`. El historial de versiones, los marcadores de borrado, Object Lock, las políticas de bucket y las reglas de ciclo de vida se quedan donde están.

En el archivo de configuración, la fuente es `old` y el destino es `new`.

## Documentación

| Guía | Qué es |
| --- | --- |
| Este archivo | Instalación, configuración, comandos, ajuste y seguridad en español |
| [README.md](README.md) | English, la misma estructura |
| [README_ZH.md](README_ZH.md) | 简体中文 |
| [README_DE.md](README_DE.md) | Deutsch |
| [README_FR.md](README_FR.md) | français |
| [README_JA.md](README_JA.md) | 日本語 |
| [README_KO.md](README_KO.md) | 한국어 |
| [README_PT.md](README_PT.md) | Portuguese |
| [README_RU.md](README_RU.md) | Русский |
| [docs/USAGE.md](docs/USAGE.md) | Acompañante breve en inglés |
| [docs/使用手册.md](docs/使用手册.md) | Manual chino más largo |
| [docs/DESIGN.md](docs/DESIGN.md) | Arquitectura y los límites que la herramienta no relaja |

`migration --help` y `migration <comando> --help` imprimen la ayuda en inglés y en chino.

## Inicio rápido

Go 1.22 o posterior. SQLite es Go puro, así que la compilación no necesita CGO.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # extremos y claves de origen y destino
CGO_ENABLED=0 go build -o bin/migration .
# o bien: make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Un `bucket:` vacío migra cada bucket de la fuente. `bucket: photos`, o `--bucket photos`, fija uno solo.

`20M` es un megabyte decimal por segundo: 20×1000×1000 bytes/s. Antes de subirlo en una red compartida, lea [Límite de tasa](#rate-limit).

## Instalación

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` ejecuta el mismo comando. El resultado es el binario estático `bin/migration`. `make test` ejecuta `go test ./...`.

Proteja `config.yaml` (`chmod 600` es un valor razonable). El proceso registra extremos y claves de objeto. No registra credenciales.

## Configuración

```bash
cp config.example.yaml config.yaml
```

El mínimo es un extremo, una access key y una secret key en cada lado. La clave de destino necesita `s3:CreateBucket` cuando el bucket aún no existe. Un bucket de destino que ya existe no se modifica.

```yaml
old:                              # origen, solo lectura
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # destino, lectura y escritura
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # vacío = cada bucket de la fuente
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # temporal de objetos pequeños; por defecto, el directorio de la base

workers: 16                       # objetos a la vez; 0 u omitido = 16
multipart_threshold: 52428800     # >= 50 MiB usa carga multipart
multipart_part_size: 16777216     # 16 MiB; debe ser >= 5 MiB (5242880)
multipart_concurrency: 4          # partes de un objeto grande; 0 u omitido = 4
rate_limit: 0                     # tope agregado de lectura en el origen; 0 u omitido = sin límite
```

`list` y `scan` necesitan el extremo y las claves de la fuente. `run`, `migrate` y `diff` necesitan ambos lados.

### Prioridad

Gana la prioridad más alta:

1. CLI: `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`
2. Variables de entorno
3. YAML
4. Valores predeterminados integrados

| Ajuste | Predeterminado si se omite |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB (52428800) |
| `multipart_part_size` | 16 MiB (16777216) |
| `rate_limit` | sin límite |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | directorio de `db_path` |
| `region` | `us-east-1` |

Ejemplo: YAML `workers: 8`, entorno `WORKERS=32` y `--workers 4` juntos usan **4**.

### Qué significa cero

`0` no significa lo mismo en cada campo.

| Campo | YAML `0` u omitido | Entorno `0` | CLI `0` |
| --- | --- | --- | --- |
| `workers` | predeterminado 16 | error | error (`--workers 0`) |
| `multipart_concurrency` | predeterminado 4 | error | error |
| `multipart_threshold` | predeterminado 50 MiB | no hay variable | no hay opción |
| `multipart_part_size` | predeterminado 16 MiB | no hay variable | no hay opción |
| `rate_limit` | sin límite | sin límite (`RATE_LIMIT=0`) | sin límite (`--rate-limit 0`) |

Una concurrencia o un tamaño negativos fallan al arrancar. `WORKERS` y `MULTIPART_CONCURRENCY` deben ser enteros simples. Los sufijos de tamaño van solo en `RATE_LIMIT`: `RATE_LIMIT=20M` es válido, `WORKERS=20M` no.

`multipart_part_size` debe ser al menos 5 MiB (5242880). S3 rechaza una parte no final más pequeña con `EntityTooSmall`. Un valor menor falla al cargar la configuración. No se eleva en silencio. Si un objeto superaría las 10 000 partes, la herramienta agranda el tamaño de parte de ese objeto para que la carga quepa.

Un nombre de bucket no puede contener espacios en blanco ni `/`.

### Variables de entorno

Una variable definida sustituye al YAML. Una variable ausente deja el archivo como está.

| Variable | Sustituye |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | extremo de origen |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | extremo de destino |
| `REGION` | cada lado cuya región sigue vacía después de las variables específicas |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | rutas locales |
| `WORKERS` `MULTIPART_CONCURRENCY` | concurrencia (entero ≥ 1) |
| `RATE_LIMIT` | tope agregado de lectura en el origen (`20M`, `20MiB`, `0`, …) |

## Comandos

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| Comando | Qué hace | Escribe en el destino | Concurrencia y tope |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | Lista todos los buckets de la fuente | No | Ninguna |
| `migration run -c config.yaml` | **Recomendado.** Inventariar y luego copiar | Sí, sin sobrescribir | El inventario es de un solo hilo. La copia usa `workers`, `multipart_concurrency` y `rate_limit` |
| `migration scan -c config.yaml` | Inventario de la fuente en SQLite | No | Un solo hilo. Sin límite de tasa |
| `migration migrate -c config.yaml` | Copia solo las filas SQLite en `pending` o `failed` | Sí, sin sobrescribir | `workers`, `multipart_concurrency` y `rate_limit` |
| `migration status -c config.yaml` | Imprime los contadores locales | No | Ninguna. Sin llamadas S3 |
| `migration diff -c config.yaml` | Lista la fuente y hace `HEAD` de cada clave en el destino | No | `workers` para HEAD en paralelo. Sin límite de tasa |
| `migration diff --extra -c config.yaml` | Lo mismo, y lista las claves que solo existen en el destino | No | `workers` |

Opciones comunes a todos los comandos:

| Opción | Significado |
| --- | --- |
| `-c`, `--config` | Archivo de configuración. Predeterminado `config.yaml` |
| `--bucket NAME` | Fija un bucket. Sustituye al YAML y a `BUCKET` |
| `--workers N` | Objetos copiados a la vez en `run` / `migrate`, y HEAD en paralelo en `diff`. Debe ser ≥ 1 |
| `--multipart-concurrency N` | Partes cargadas a la vez dentro de un objeto grande. Debe ser ≥ 1. Lo ignoran `list`, `scan`, `status` y `diff` |
| `--rate-limit SIZE` | Tope agregado de lectura de cuerpos de objeto en `run` / `migrate`. `0` es sin límite |
| `--version` | Imprime la versión |
| `--help` | Ayuda bilingüe |

`diff` también acepta `--extra`.

`status` imprime `BUCKET`, `TOTAL`, `PENDING`, `DONE`, `CONFLICT`, `SKIPPED` y `FAILED`. Sin un bucket fijado, lee `migration-*.db` junto a `db_path`, y la base configurada si ese archivo ya está ligado a un bucket. Si aún no hay nada, lo dice y sale con 0. Ejecute antes `scan` o `run`.

## Flujos

### Migrar todos los buckets

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Para cada bucket, `run` hace lo siguiente:

1. `HeadBucket` en la fuente.
2. Crea el bucket en el destino si falta. Un bucket existente no se modifica.
3. `ListObjectsV2` de la fuente hacia SQLite. Este paso es de un solo hilo.
4. Arranca `workers` goroutines de copia. Cada objeto se comprueba primero con `HeadObject` en el destino. Una clave ausente se lee de la fuente y se escribe en el destino. Esas lecturas comparten `rate_limit`. Una clave que ya coincide con el inventario se marca `done` y no se sube. Una clave presente que no coincide se añade al registro de conflictos y no se sobrescribe.
5. Un objeto a partir de `multipart_threshold` sube sus partes con `multipart_concurrency`. Esas partes comparten el mismo tope de bytes.
6. Comprueba el tamaño en el destino. Un objeto de una sola parte también se comprueba por ETag cuando los ETag son comparables. Véase [Estado de los objetos](#resume-and-object-state).

Los buckets se copian uno tras otro, así que `rate_limit` es el tope de todo el proceso.

### Un solo bucket

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### Objetos escritos después en la fuente

`ListObjectsV2` no puede filtrar por fecha de modificación en el servidor. Ponerse al día es otro listado completo. Las claves nuevas pasan a `pending`. Las filas `failed` o `skipped` vuelven a `pending` cuando se vuelven a listar. `done` y `conflict` se quedan como están, así que un segundo pase no las reenvía ni sobrescribe el destino.

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

Volver a ejecutar `run` es esos dos pasos en un solo comando.

### Inventariar primero

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # recuentos de objetos, sin llamadas S3
./bin/migration migrate -c config.yaml --workers 16
```

## Concurrencia

Tres controles, tres tareas distintas:

| Control | Qué limita | Súbalo cuando | Bájelo cuando |
| --- | --- | --- | --- |
| `workers` / `--workers` | Cuántos **objetos** están en vuelo. También el paralelismo HEAD de `diff` | Sobre todo objetos pequeños, y CPU, disco y proxy aún tienen margen | 503, tiempos de espera, un disco de origen saturado, o está depurando un objeto (`--workers 1`) |
| `multipart_concurrency` / `--multipart-concurrency` | Cuántas **partes de un objeto grande** se suben a la vez | Un solo objeto es grande y ese flujo aún puede usar el enlace | Los objetos grandes ya llenan el enlace, o varios juntos llenan el proxy |
| `rate_limit` / `--rate-limit` | Cuántos **bytes de origen por segundo** pueden leer todos esos workers, juntos | La fuente y la red aún tienen margen | Producción usa el clúster de origen o la misma NIC. Empiece en `20M` y luego pruebe `30M` o `50M` |

`scan` ignora `workers` y `rate_limit`. El listado va página a página porque cada página necesita el token de continuación de la anterior, y las filas van a un solo archivo SQLite. Un escaneo lento no se arregla con `--workers`. `list`, `diff` y `status` no leen cuerpos de objeto, así que el tope de bytes no les aplica. `diff` sí usa `--workers` para HEAD.

```text
workers = 4 objetos a la vez
  objeto A (pequeño)  ── un GET + PUT
  objeto B (pequeño)  ── un GET + PUT
  objeto C (grande)   ── multipart_concurrency GET por rango + UploadPart
  objeto D (grande)   ── lo mismo
```

El pico de peticiones en vuelo contra un clúster es aproximadamente `workers × multipart_concurrency`. Un objeto pequeño usa una petición. Un objeto en el umbral multipart multiplica por la concurrencia de partes. El predeterminado `16 × 4` es 64. El `maxconn` de HAProxy o Nginx, y los discos de ambos clústeres, tienen que soportar ese producto.

El cliente HTTP sube las conexiones inactivas por host hasta ese producto (con tope en 1024), en lugar de las dos conexiones inactivas por host del valor predeterminado de Go. Eso es reutilización de conexiones. El trabajo en vuelo lo acotan el grupo de workers y el semáforo de partes. La cola de espera guarda como mucho 2048 descriptores de objeto, así que un `--workers` grande no carga el bucket entero en memoria.

Puntos de partida:

| Carga | Sugerencia |
| --- | --- |
| Muchos objetos pequeños (kilobytes a unos pocos megabytes), el enlace tiene margen | `workers` 16–64, deje `multipart_concurrency` en 4 |
| Tamaños mezclados | `workers` 8–32, `multipart_concurrency` 4–8 |
| Unos pocos objetos muy grandes | `workers` 2–8, `multipart_concurrency` 4–16 |
| Un objeto falla y el registro es difícil de seguir | `--workers 1` |
| El proxy devuelve 503, tiempos de espera, o el disco de origen está lleno | Baje primero `workers`. Si lo lento son objetos grandes, baje también `multipart_concurrency` |
| Reservar ancho de banda para producción | `--rate-limit 20M`, luego `30M` o `50M` cuando la fuente se vea sana. `0` quita el tope |
| Más rápido con objetos pequeños | Suba `workers` |
| Más rápido con un objeto grande | Suba `multipart_concurrency`, o baje `multipart_part_size` (bytes, mínimo 5 MiB) |

Durante una copia se registra una línea de progreso cada unos 10 segundos: processed, copied, conflicts, skipped, failed y bytes.

Cada objeto pequeño en vuelo tiene un archivo temporal en `spool_dir`, de hasta unos `multipart_threshold` bytes, porque `PutObject` necesita un cuerpo en el que se pueda hacer seek. Si `/tmp` es un disco en memoria, apunte `spool_dir` a un disco real. Más workers significa más de esos archivos a la vez.

Un proceso por archivo de base de datos. SQLite se abre en modo WAL con una sola conexión dentro del proceso, así que las actualizaciones de estado quedan en serie y los bytes del objeto siguen en el grupo de workers. Un segundo proceso sobre el mismo archivo sigue informando `database is locked`.

<a id="rate-limit"></a>

## Límite de tasa

`rate_limit` es un solo cubo de tokens compartido por cada worker de copia, incluidas las partes de un objeto grande. Cuenta los bytes leídos de los cuerpos de objeto de la fuente. List, HEAD y la comprobación de conflicto antes de escribir quedan fuera del tope. La carga que sigue a una lectura terminada no se cuenta una segunda vez.

Un objeto pequeño se lee de la fuente al tope y luego se escribe en el destino. Las lecturas de la fuente se mantienen dentro del tope. La escritura siguiente puede ser brevemente más rápida. Si el host de migración comparte una NIC con producción, esa escritura sigue usando ancho de banda. Cuando el uso total de la NIC deba quedarse cerca de 40 MB/s, empiece en `20M`.

El tope solo ralentiza las copias. No sobrescribe y no elimina. Un objeto que ya coincide con el inventario se sigue omitiendo. El primer Ctrl-C cancela una lectura que espera tokens. El objeto sin terminar permanece en SQLite para el siguiente `migrate`.

El tope no reduce cuántas peticiones están en vuelo. Un proxy que devuelve 503 sigue siendo un problema de concurrencia: baje `workers` y después `multipart_concurrency`.

### Unidades

Los sufijos no distinguen mayúsculas. Un `/s` final se acepta y se ignora. Un número solo son bytes por segundo. `1.5M` son 1 500 000 bytes/s.

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
| `0`, `0M` u omitido | sin límite |

`M` es decimal. `Mi` es binario. `20M` está cerca de un 5 % por debajo de `20MiB` (20000000 frente a 20971520). Un valor negativo o un sufijo desconocido falla al arrancar.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # sin límite, a propósito
```

<a id="resume-and-object-state"></a>

## Reanudación y estado de los objetos

El primer Ctrl-C o `SIGTERM` cancela el grupo de workers. Los objetos que no han empezado siguen en `pending`. Una carga multipart en curso se aborta, lo que solo libera esa carga y no borra un objeto confirmado. El estado ya escrito en SQLite se conserva. Una segunda señal sale de inmediato con el código 130.

Si el `PutObject` en el destino tuvo éxito y la fila aún no estaba marcada `done`, la fila se queda en `failed` (o `pending`) aunque el objeto ya esté en el destino. El siguiente `migrate` comprueba el destino con `HeadObject`. Cuando el tamaño coincide y cualquier ETag comparable coincide, la fila pasa a `done`. Cuando el tamaño difiere, o un ETag comparable difiere, la fila pasa a `conflict`, y el objeto sigue sin sobrescribirse.

Una petición cancelada, un plazo y `request canceled` son `failed`, aunque el texto del error también contenga `NotFound` o `StatusCode: 404`. Un aborto multipart suele informar `NoSuchUpload`. Eso no es un objeto de origen ausente. `skipped` significa que la petición a la fuente terminó y el objeto realmente ya no estaba.

```bash
./bin/migration migrate -c config.yaml
```

`migrate` solo reintenta `pending` y `failed`.

| Estado | Significado | Un escaneo posterior |
| --- | --- | --- |
| `pending` | Esperando la copia | Actualiza tamaño y ETag |
| `failed` | El último intento falló. `migrate` lo reintenta. El mensaje está en `last_error` | Devuelve la fila a `pending` |
| `skipped` | La petición a la fuente terminó con 404 / `NoSuchKey` | Vuelve a `pending` si la clave se lista de nuevo |
| `done` | Copiado y comprobado, o el destino ya coincidía con el inventario | Sin cambios |
| `conflict` | El destino tiene la clave y no coincide con el inventario. No se sobrescribió | Sin cambios |

**Regla de coincidencia.** El tamaño debe ser igual al tamaño inventariado. Los ETag solo se comparan cuando ambos lados tienen uno y ninguno es un ETag multipart (un valor que contiene `-`, con forma `hash-N`). Los ETag multipart dependen de los límites de las partes, así que no se comparan; la comprobación es el tamaño. Un tamaño distinto, o un ETag comparable distinto, es un conflicto.

Cada conflicto es una línea TSV en el registro, con marcas de tiempo RFC3339 UTC:

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

Un solo bucket configurado usa `db_path` y `conflict_log` tal como están escritos. El modo de todos los buckets deriva archivos vecinos:

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

Un archivo de base está ligado a un bucket. Apuntarlo a otro bucket falla con `db_path is bound to bucket "…"`.

## Comprobar el destino con diff

`diff` lista el bucket de origen en vivo y hace `HEAD` de cada clave en el destino. No lee el inventario SQLite de la migración y no sube nada.

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

La segunda línea de resumen repite los contadores en chino, porque así los imprime el comando. Se muestran como mucho 50 líneas de incidencia; el resto se cuenta en una línea final «and N more».

| Resultado | Significado |
| --- | --- |
| `missing` | En la fuente, no en el destino. Vuelva a ejecutar `migrate` o `run` |
| `size` / `etag` | La misma clave, el contenido no coincide. A menudo una escritura en vivo en el destino. Esta herramienta no la sobrescribe |
| `extra` | Solo en el destino. Se informa con `--extra`. Suele ser una escritura que ya llegó al clúster nuevo. No es un fallo |

El código de salida es 1 cuando falta alguna clave o no coinciden el tamaño o el ETag. `extra_on_target` no hace fallar el comando. Con varios buckets, un hueco falla ese bucket y el comando sigue con el resto, y sale con 1 si algún bucket falló.

## Seguridad

| Garantía | Cómo |
| --- | --- |
| No sobrescribir un objeto de destino | `HeadObject` antes de cada escritura. Una coincidencia es `done`. Una diferencia es `conflict.log` y un salto |
| No eliminar | No hay `DeleteObject`. Una carga multipart fallida solo se aborta |
| No mezclar buckets | `HeadBucket`, una comprobación del nombre de bucket en cada página de listado, y un nombre de bucket guardado en el archivo SQLite |
| Borrados en la fuente durante la ejecución | Un 404 de origen ya terminado es `skipped`. Cancelación y tiempo de espera son `failed` y se reintentarán |
| Reanudar tras caída y cancelación | El estado de cada objeto está en un archivo SQLite WAL. La primera señal cancela los workers; el siguiente `migrate` continúa |
| Memoria acotada | Los objetos pequeños van a disco. Los grandes se leen por partes. La cola de trabajos tiene tope |
| Comprobación tras la copia | Siempre el tamaño. También el ETag en un objeto de una sola parte cuando ambos ETag son comparables |

Fuera de alcance: historial de versiones, marcadores de borrado, Object Lock, políticas de bucket, reglas de ciclo de vida, sincronización en dos sentidos y `CopyObject` en el servidor (los clústeres están separados, así que los bytes pasan por este proceso).

## Resolución de problemas

| Lo que se ve | Qué hacer |
| --- | --- |
| `read config … copy config.example.yaml first` | Falta la ruta de `-c`. Copie `config.example.yaml` |
| `old.endpoint is required` | Defina `old.endpoint` o `OLD_ENDPOINT`. El lado de destino informa igual `new.endpoint is required`. Las claves informan `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`, `WORKERS` o el `workers` del YAML es menor que 1. Un `0` en YAML significa «predeterminado 16». Un `0` en la CLI o en el entorno es un error |
| `multipart_concurrency must be >= 1` | La misma regla, para la concurrencia de partes. Un `0` en YAML significa «predeterminado 4» |
| `env WORKERS="…" is not an integer` | `WORKERS` debe ser un entero sin unidades. El ancho de banda va en `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | Las partes no finales deben ser de al menos 5 MiB. El predeterminado de 16 MiB ya es válido. El valor no se eleva solo |
| `unknown size suffix` o `rate limit` | No se reconoció `rate_limit`, `RATE_LIMIT` o `--rate-limit`. El decimal es `20M`. El binario es `20Mi` o `20MiB` |
| `head bucket "…"` | Falta el bucket, el extremo es incorrecto o la clave no puede leer. Un fallo al crear en el destino dice `create bucket "…" on target (need s3:CreateBucket)` |
| La fuente sigue ocupada con `20M` | `20M` son 20000000 bytes/s de cuerpos leídos en la fuente. La carga queda fuera de ese tope. En una NIC compartida, baje el número |
| `scan` o `diff` no se ralentizaron | Esperado. El tope se aplica mientras `run` / `migrate` leen cuerpos de objeto |
| El escaneo sigue lento tras subir `--workers` | Esperado. `scan` es de un solo hilo. La concurrencia empieza en la copia, y en el HEAD de `diff` |
| Muchos 503 o tiempos de espera | Baje `workers`. Con objetos grandes, baje `multipart_concurrency` para que `workers × multipart_concurrency` quepa en el `maxconn` del proxy |
| Muchos resultados `etag mismatch` | El SSE o la compresión en la fuente pueden cambiar el ETag. Baje `multipart_threshold` para ir por multipart (se comprueba el tamaño, no se compara el ETag), o muestree los objetos a mano |
| `db_path is bound to bucket "…"` | Ese archivo ya pertenece a otro bucket. Use otro `db_path` |
| `database is locked` | Dos procesos escriben la misma base. Un proceso por archivo |
| El disco se llena de temporales | Mueva `spool_dir` a un disco grande, o baje `workers` |
| Ctrl-C no vuelve | Espere la petición en curso, o púlselo otra vez (salida 130) |
| `diff` sigue informando `missing` | Vuelva a ejecutar `migrate` y lea `last_error` de las filas `failed` |

## Códigos de salida

| Código | Significado |
| --- | --- |
| 0 | Éxito. Con un solo bucket, el primer Ctrl-C durante la copia (`migrate`, o la mitad de copia de `run`) también sale con 0. Los objetos sin terminar quedan en SQLite |
| 1 | Configuración incorrecta, un error remoto, un bucket fallido, o `diff` encontró una clave ausente o distinta. También el primer Ctrl-C durante `scan`, y una interrupción mientras hay más de un bucket en la ejecución |
| 130 | Un segundo `SIGINT` o `SIGTERM`. El proceso sale de inmediato |

Los errores del comando se imprimen en stderr como `error: …`.

## Licencia

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
