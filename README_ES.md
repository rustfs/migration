# migration

[English](README.md) | [简体中文](README_ZH.md) | [Deutsch](README_DE.md) | **Español** | [Français](README_FR.md) | [日本語](README_JA.md) | [한국어](README_KO.md) | [Português](README_PT.md) | [Русский](README_RU.md)

`migration` copia la versión actual de los objetos de un clúster compatible con S3 a otro. Por ejemplo, permite recuperar los datos históricos de un clúster RustFS o MinIO mientras las aplicaciones ya escriben en el nuevo. En la configuración, `old` identifica el origen y `new` el destino. Ambos usan direcciones S3 con el nombre del bucket en la ruta.

La herramienta copia las claves que faltan, anota los conflictos en `conflict.log` y guarda el progreso en una base de datos SQLite local. No elimina objetos. El destino debe respetar `If-None-Match: *` tanto en `PutObject` como en `CompleteMultipartUpload`. Así se conservan incluso los objetos que otra aplicación cree después de la comprobación inicial.

Se copian el contenido actual del objeto, `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` y los metadatos de usuario `x-amz-meta-*`. Quedan fuera el historial de versiones, los marcadores de eliminación, Object Lock, las políticas de los buckets y las reglas del ciclo de vida.

## Documentación

[Guía rápida en inglés](docs/USAGE.md) · [Manual de operación en chino](docs/使用手册.md) · [Arquitectura en inglés](docs/DESIGN.md)

## Inicio rápido

Para compilar desde el repositorio se necesita Go 1.22 o posterior. Una vez copiada la plantilla, introduzca las direcciones y las credenciales de ambos clústeres en `config.yaml`.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
# o bien: make build
cp config.example.yaml config.yaml   # extremos y claves de origen y destino
```

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Un `bucket:` vacío migra cada bucket de la fuente. `bucket: photos`, o `--bucket photos`, fija uno solo.

Use `--bucket ""` para seleccionar todos los buckets del origen aunque el archivo YAML o `BUCKET` especifiquen uno.

Con `--rate-limit 20M`, todos los procesos de copia leen en conjunto hasta 20 000 000 de bytes por segundo del origen. Consulte el [límite de ancho de banda](#rate-limit) para ver las demás unidades.

## Instalación

Abra una ejecución correcta de [Build packages](https://github.com/rustfs/migration/actions/workflows/packages.yml) y elija su plataforma en **Artifacts**. Descomprima la descarga, verifique el archivo con la suma SHA-256 incluida y extraiga su contenido. Los paquetes para Linux (amd64 y arm64) se distribuyen en formato `.tar.gz`. El de Windows (amd64) es un `.zip` que contiene `migration.exe`.

```bash
./migration --version
```

```powershell
.\migration.exe --version
```

Proteja `config.yaml` (`chmod 600` es un valor razonable). El proceso registra extremos y claves de objeto. No registra credenciales.

## Configuración

```bash
cp config.example.yaml config.yaml
```

Indique la dirección HTTP o HTTPS y las credenciales de cada clúster. Use los campos de credenciales correspondientes. La URL no admite credenciales, parámetros de consulta ni fragmentos. Para crear un bucket se necesita el permiso `s3:CreateBucket` en el destino.

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

`multipart_part_size` admite de 5 MiB a 5 GiB, con un valor predeterminado de 16 MiB. Los objetos de más de 5 GiB siempre se cargan por partes. La herramienta aumenta su tamaño si hacen falta más de 10 000 partes. Si aun así se superan los 5 GiB por parte, la transferencia falla.

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

## Flujos de trabajo

### Migrar todos los buckets

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Para cada bucket, `run` hace lo siguiente:

1. `HeadBucket` en la fuente.
2. `ListObjectsV2` de la fuente hacia SQLite. Este paso es de un solo hilo.
3. Crea el bucket en el destino si falta. Un bucket existente no se modifica.
4. Se copian hasta `workers` objetos a la vez. Si otra aplicación crea la clave después del primer HEAD, la escritura condicional protege ese objeto. La herramienta vuelve a comprobar la clave y registra `done` o `conflict`.
5. A partir de `multipart_threshold`, se transfieren hasta `multipart_concurrency` partes de cada objeto en paralelo. Todas las lecturas comparten el mismo límite de ancho de banda.
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

`workers` controla cuántos objetos se copian a la vez. Para objetos grandes, `multipart_concurrency` determina además cuántas partes de cada objeto se pueden transferir en paralelo. Con los valores predeterminados de 16 objetos y cuatro partes puede haber unas 64 solicitudes simultáneas por clúster. Tenga en cuenta el rendimiento de los discos, el proxy y la memoria disponible.

`scan` ignora `workers` y `rate_limit`. El listado va página a página porque cada página necesita el token de continuación de la anterior, y las filas van a un solo archivo SQLite. Un escaneo lento no se arregla con `--workers`. `list`, `diff` y `status` no leen cuerpos de objeto, así que el tope de bytes no les aplica. `diff` sí usa `--workers` para HEAD.

```text
workers = 4 objetos a la vez
  objeto A (pequeño)  ── un GET + PUT
  objeto B (pequeño)  ── un GET + PUT
  objeto C (grande)   ── multipart_concurrency GET por rango + UploadPart
  objeto D (grande)   ── lo mismo
```

Cada parte en curso se almacena en memoria. La cola admite hasta 2048 descriptores de objetos y el cliente HTTP puede mantener hasta 1024 conexiones inactivas por servidor para reutilizarlas.

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

## Límite de ancho de banda

`rate_limit` limita el caudal total de lectura del origen, incluidas todas las partes de los objetos. Las consultas de claves y cabeceras y las cargas al destino quedan fuera de este límite.

Los objetos pequeños se leen primero en el disco local y después se cargan en el destino. Si ambas operaciones comparten la interfaz de red, reserve ancho de banda para la carga al fijar el límite de lectura.

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

Si el ETag de origen cambia entre el inventario y la copia, la transferencia falla. Actualice el inventario con `scan` y repita `migrate`, o vuelva a ejecutar `run`.

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

Si la clave ya existe, solo se marca como `done` cuando coincide el tamaño y ambos ETag tienen el mismo valor no vacío. En los demás casos se registra `conflict` y se conserva el objeto de destino. El mismo contenido puede tener distintos ETag si se dividió en partes de otro tamaño. Revise estos conflictos si una carga terminó justo antes de una interrupción.

Cada conflicto es una línea TSV en el registro, con marcas de tiempo RFC3339 UTC:

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

Las claves que contienen caracteres de control o empiezan por comillas dobles se escriben entre comillas con las secuencias de escape de Go.

Un solo bucket configurado usa `db_path` y `conflict_log` tal como están escritos. El modo de todos los buckets deriva archivos vecinos:

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

`migrate` necesita un inventario creado con `scan` o `run`. La base de datos queda vinculada al bucket y a los puntos de conexión. Antes de reutilizar una base de una versión anterior, confirme que corresponde a estos clústeres. El primer escaneo registra el origen y la primera migración, el destino. Si el escaneo falla o se interrumpe, vuelva a ejecutar `scan` o `run` antes de usar `migrate`. Las bases de versiones anteriores no registran si el escaneo terminó. En caso de duda, repítalo.

## Comprobar el destino con diff

`diff` comprueba tamaños y ETag comparables sin descargar objetos. El contador `size_only` indica cuántos de los objetos incluidos en `ok` se comprobaron solo por tamaño. Con `--extra` se crea un índice temporal para identificar las claves exclusivas del destino. Si el bucket de origen está vacío, `diff` comprueba que el bucket de destino exista y devuelve el código 1 si falta.

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

La segunda línea de resumen repite los contadores en chino, porque así los imprime el comando. Se muestran como mucho 50 líneas de incidencia; el resto se cuenta en una línea final «and N more».

| Resultado | Significado |
| --- | --- |
| `missing` | La clave existe en el origen y falta en el destino |
| `size` / `etag` | La misma clave, el contenido no coincide. A menudo una escritura en vivo en el destino. Esta herramienta no la sobrescribe |
| `extra` | Solo en el destino. Se informa con `--extra`. Suele ser una escritura que ya llegó al clúster nuevo. No es un fallo |

El código de salida es 1 cuando falta alguna clave o no coinciden el tamaño o el ETag. `extra_on_target` no hace fallar el comando. Con varios buckets, un hueco falla ese bucket y el comando sigue con el resto, y sale con 1 si algún bucket falló.

## Seguridad

| Garantía | Cómo |
| --- | --- |
| No sobrescribir un objeto de destino | `HeadObject` → `PutObject` / `CompleteMultipartUpload` (`If-None-Match: *`) |
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
| Muchos resultados `etag mismatch` | Compruebe por separado el contenido del objeto. El cifrado del servidor y la división en partes pueden afectar al ETag. |
| `db_path is bound to bucket "…"` | Ese archivo ya pertenece a otro bucket. Use otro `db_path` |
| `database is locked` | Dos procesos escriben la misma base. Un proceso por archivo |
| El disco se llena de temporales | Mueva `spool_dir` a un disco grande, o baje `workers` |
| Ctrl-C no vuelve | Espere la petición en curso, o púlselo otra vez (salida 130) |
| `diff` sigue informando `missing` | Si el estado es `failed`, consulte `last_error` antes de reintentar. Para `done` o `conflict`, cree otro inventario en un directorio aparte. En el modo de todos los buckets no basta con cambiar el nombre del archivo de base de datos. |

## Códigos de salida

| Código | Significado |
| --- | --- |
| 0 | Operación completada. Los conflictos y los objetos omitidos se contabilizan por separado. |
| 1 | Configuración no válida, interrupción, error de operación o de copia, o claves ausentes o discrepantes detectadas por `diff`. |
| 130 | Un segundo `SIGINT` o `SIGTERM`. El proceso sale de inmediato |

Los errores del comando se imprimen en stderr como `error: …`.

## Licencia

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
