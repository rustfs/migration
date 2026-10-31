# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ZH.md">简体中文</a> |
  Deutsch |
  <a href="https://github.com/rustfs/migration/blob/main/README_ES.md">Español</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_FR.md">français</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_JA.md">日本語</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_KO.md">한국어</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_PT.md">Portuguese</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_RU.md">Русский</a>
</p>

`migration` kopiert Objekte in eine Richtung zwischen zwei S3-kompatiblen Clustern. Gedacht ist es für selbst betriebenes RustFS und MinIO sowie für jeden anderen Endpunkt, der die S3-API spricht. Anfragen verwenden immer Path-Style-Adressierung, wie es bei diesen Clustern üblich ist.

Der Lauf darf neben produktivem Verkehr stehen. Für jeden schreibenden Befehl gelten drei Regeln:

- **Niemals überschreiben.** Vor jedem Schreiben ruft das Werkzeug `HeadObject` auf dem Ziel auf. Eine Übereinstimmung gilt als erledigt. Ein abweichendes Objekt wird nur protokolliert und bleibt unangetastet.
- **Niemals löschen.** Es gibt keinen `DeleteObject`-Aufruf. Der Abbruch eines Multipart-Uploads verwirft nur diesen unvollständigen Upload, nicht ein bereits committetes Objekt.
- **Fortsetzbar.** Jedes Objekt hat eine Zeile in einer lokalen SQLite-Datenbank. Anhalten und neu starten wiederholt abgeschlossene Arbeit nicht.

Übliches Layout: Der alte Cluster hält noch die Historie, der neue Cluster nimmt alle Schreibvorgänge an, und die Anwendung liest bereits von beiden. Dieses Werkzeug füllt Schlüssel auf, die das Ziel noch nicht hat. Es ersetzt ein neueres Objekt auf dem Ziel nicht durch ältere Bytes von der Quelle. Konflikte beim selben Schlüssel sind in diesem Layout erwartet. `conflict.log` lesen und von Hand auflösen.

Kopiert wird das aktuelle Objekt, dazu `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` und `x-amz-meta-*`-Benutzermetadaten. Versionshistorie, Delete-Marker, Object Lock, Bucket-Policies und Lifecycle-Regeln bleiben, wo sie sind.

In der Konfiguration ist die Quelle `old` und das Ziel `new`.

## Dokumentation

| Anleitung | Inhalt |
| --- | --- |
| Diese Datei | Deutsch: Installation, Konfiguration, Befehle, Tuning und Sicherheit |
| [README.md](README.md) | English, gleiche Struktur |
| [README_ZH.md](README_ZH.md) | 简体中文 |
| [README_FR.md](README_FR.md) | français |
| [README_JA.md](README_JA.md) | 日本語 |
| [README_ES.md](README_ES.md) | Español |
| [README_KO.md](README_KO.md) | 한국어 |
| [README_PT.md](README_PT.md) | Portuguese |
| [README_RU.md](README_RU.md) | Русский |
| [docs/USAGE.md](docs/USAGE.md) | Kurze englische Begleitseite |
| [docs/使用手册.md](docs/使用手册.md) | Ausführlicheres chinesisches Handbuch |
| [docs/DESIGN.md](docs/DESIGN.md) | Architektur und die Grenzen, die das Werkzeug nicht lockert |

`migration --help` und `migration <Befehl> --help` drucken die Hilfe zweisprachig auf Englisch und Chinesisch.

## Schnellstart

Go 1.22 oder neuer. SQLite ist reines Go, der Build braucht kein CGO.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # Endpunkte und Schlüssel von Quelle und Ziel
CGO_ENABLED=0 go build -o bin/migration .
# oder: make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Ein leeres `bucket:` migriert jeden Bucket der Quelle. `bucket: photos` oder `--bucket photos` legt einen Bucket fest.

`20M` ist ein dezimales Megabyte pro Sekunde: 20×1000×1000 Bytes/s. Vor dem Erhöhen in einem geteilten Netz [Ratenlimit](#rate-limit) lesen.

## Installation

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` führt denselben Befehl aus. Das Ergebnis ist die statische Binary `bin/migration`. `make test` führt `go test ./...` aus.

`config.yaml` schützen (`chmod 600` ist ein sinnvoller Default). Der Prozess protokolliert Endpunkte und Objektschlüssel. Zugangsdaten protokolliert er nicht.

## Konfiguration

```bash
cp config.example.yaml config.yaml
```

Mindestens Endpunkt, Access Key und Secret Key für beide Seiten. Der Ziel-Key braucht `s3:CreateBucket`, wenn der Bucket dort noch fehlt. Ein vorhandener Ziel-Bucket bleibt unverändert.

```yaml
old:                              # Quelle, nur lesen
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # Ziel, lesen und schreiben
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # leer = jeder Bucket der Quelle
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # Temp-Verzeichnis für kleine Objekte; Default ist das DB-Verzeichnis

workers: 16                       # Objekte gleichzeitig; 0 oder weglassen = 16
multipart_threshold: 52428800     # >= 50 MiB nutzt Multipart-Upload
multipart_part_size: 16777216     # 16 MiB; muss >= 5 MiB sein (5242880)
multipart_concurrency: 4          # Parts eines großen Objekts; 0 oder weglassen = 4
rate_limit: 0                     # gemeinsame Lesegrenze an der Quelle; 0 oder weglassen = unbegrenzt
```

`list` und `scan` brauchen Endpunkt und Schlüssel der Quelle. `run`, `migrate` und `diff` brauchen beide Seiten.

### Vorrang

Die höchste Priorität gewinnt:

1. CLI: `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`
2. Umgebungsvariablen
3. YAML
4. Eingebaute Defaults

| Einstellung | Default, wenn weggelassen |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB (52428800) |
| `multipart_part_size` | 16 MiB (16777216) |
| `rate_limit` | unbegrenzt |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | Verzeichnis von `db_path` |
| `region` | `us-east-1` |

Beispiel: YAML `workers: 8`, Umgebung `WORKERS=32` und `--workers 4` zusammen ergeben **4**.

### Was Null bedeutet

`0` ist nicht bei jedem Feld dasselbe.

| Feld | YAML `0` oder weggelassen | Umgebung `0` | CLI `0` |
| --- | --- | --- | --- |
| `workers` | Default 16 | Fehler | Fehler (`--workers 0`) |
| `multipart_concurrency` | Default 4 | Fehler | Fehler |
| `multipart_threshold` | Default 50 MiB | keine Variable | kein Flag |
| `multipart_part_size` | Default 16 MiB | keine Variable | kein Flag |
| `rate_limit` | unbegrenzt | unbegrenzt (`RATE_LIMIT=0`) | unbegrenzt (`--rate-limit 0`) |

Negative Nebenläufigkeit oder Größe bricht den Start ab. `WORKERS` und `MULTIPART_CONCURRENCY` müssen reine Ganzzahlen sein. Größensuffixe gehören nur an `RATE_LIMIT`: `RATE_LIMIT=20M` ist gültig, `WORKERS=20M` nicht.

`multipart_part_size` muss mindestens 5 MiB betragen (5242880). S3 lehnt ein kleineres Nicht-Schluss-Part mit `EntityTooSmall` ab. Ein kleinerer Wert scheitert beim Laden der Konfiguration. Er wird nicht still angehoben. Braucht ein Objekt mehr als 10.000 Parts, vergrößert das Werkzeug die Part-Größe dieses Objekts, damit der Upload passt.

Ein Bucket-Name darf weder Whitespace noch `/` enthalten.

### Umgebungsvariablen

Gesetzte Variablen überschreiben YAML. Ungesetzte Variablen lassen die Datei in Ruhe.

| Variable | Überschreibt |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | Quell-Endpunkt |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | Ziel-Endpunkt |
| `REGION` | jede Seite, deren Region nach den spezifischen Variablen noch leer ist |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | lokale Pfade |
| `WORKERS` `MULTIPART_CONCURRENCY` | Nebenläufigkeit (Ganzzahl ≥ 1) |
| `RATE_LIMIT` | gemeinsame Lesegrenze an der Quelle (`20M`, `20MiB`, `0`, …) |

## Befehle

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| Befehl | Wirkung | Schreibt auf das Ziel | Nebenläufigkeit und Limit |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | Alle Buckets der Quelle auflisten | Nein | Keine |
| `migration run -c config.yaml` | **Empfohlen.** Scannen, dann kopieren | Ja, ohne zu überschreiben | Scan ist einsträngig. Die Kopie nutzt `workers`, `multipart_concurrency` und `rate_limit` |
| `migration scan -c config.yaml` | Inventar der Quelle nach SQLite | Nein | Einsträngig. Kein Ratenlimit |
| `migration migrate -c config.yaml` | Nur SQLite-Zeilen in `pending` oder `failed` kopieren | Ja, ohne zu überschreiben | `workers`, `multipart_concurrency` und `rate_limit` |
| `migration status -c config.yaml` | Lokale Zähler ausgeben | Nein | Keine. Keine S3-Aufrufe |
| `migration diff -c config.yaml` | Quelle listen und jeden Schlüssel auf dem Ziel per `HEAD` prüfen | Nein | `workers` für parallele HEAD. Kein Ratenlimit |
| `migration diff --extra -c config.yaml` | Dasselbe, plus Schlüssel nur auf dem Ziel | Nein | `workers` |

Flags für jeden Befehl:

| Flag | Bedeutung |
| --- | --- |
| `-c`, `--config` | Konfigurationsdatei. Default `config.yaml` |
| `--bucket NAME` | Einen Bucket festlegen. Überschreibt YAML und `BUCKET` |
| `--workers N` | Gleichzeitige Objekte für `run` / `migrate` und parallele HEAD für `diff`. Muss ≥ 1 sein |
| `--multipart-concurrency N` | Gleichzeitige Parts innerhalb eines großen Objekts. Muss ≥ 1 sein. Wird von `list`, `scan`, `status` und `diff` ignoriert |
| `--rate-limit SIZE` | Gemeinsame Lesegrenze für Objektkörper in `run` / `migrate`. `0` ist unbegrenzt |
| `--version` | Version ausgeben |
| `--help` | Zweisprachige Hilfe |

`diff` akzeptiert zusätzlich `--extra`.

`status` druckt `BUCKET`, `TOTAL`, `PENDING`, `DONE`, `CONFLICT`, `SKIPPED` und `FAILED`. Ohne festgelegten Bucket liest es `migration-*.db` neben `db_path` und die konfigurierte Datenbank selbst, wenn diese Datei schon an einen Bucket gebunden ist. Liegt noch nichts vor, sagt es das und endet mit 0. Zuerst `scan` oder `run` ausführen.

## Abläufe

### Alle Buckets migrieren

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Für jeden Bucket macht `run` Folgendes:

1. `HeadBucket` auf der Quelle.
2. Den Bucket auf dem Ziel anlegen, wenn er fehlt. Ein vorhandener Bucket wird nicht verändert.
3. `ListObjectsV2` der Quelle nach SQLite. Dieser Schritt ist einsträngig.
4. `workers` Kopier-Goroutinen starten. Jedes Objekt wird zuerst mit `HeadObject` auf dem Ziel geprüft. Ein fehlender Schlüssel wird von der Quelle gelesen und auf das Ziel geschrieben. Diese Lesevorgänge teilen sich `rate_limit`. Ein Schlüssel, der schon zum Inventar passt, wird `done` und nicht hochgeladen. Ein vorhandener, nicht passender Schlüssel wird an das Konfliktlog angehängt und nicht überschrieben.
5. Ein Objekt ab `multipart_threshold` lädt seine Parts mit `multipart_concurrency`. Diese Parts teilen dieselbe Bytegrenze.
6. Die Größe auf dem Ziel prüfen. Ein Single-Part-Objekt wird zusätzlich per ETag geprüft, wenn die ETags vergleichbar sind. Siehe [Objektstatus](#resume-and-object-state).

Buckets werden nacheinander kopiert, deshalb ist `rate_limit` die Grenze des ganzen Prozesses.

### Ein Bucket

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### Später auf der Quelle geschriebene Objekte

`ListObjectsV2` kann serverseitig nicht nach Änderungszeit filtern. Nachziehen ist ein weiteres vollständiges Listing. Neue Schlüssel werden `pending`. Zeilen in `failed` oder `skipped` kehren nach `pending` zurück, wenn sie wieder gelistet werden. `done` und `conflict` bleiben stehen, ein zweiter Durchlauf lädt sie nicht neu hoch und überschreibt das Ziel nicht.

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

`run` noch einmal ist dieselben zwei Schritte in einem Befehl.

### Zuerst inventarisieren

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # Objektzähler, keine S3-Aufrufe
./bin/migration migrate -c config.yaml --workers 16
```

## Nebenläufigkeit

Drei Regler, drei verschiedene Aufgaben:

| Regler | Was er begrenzt | Erhöhen, wenn | Senken, wenn |
| --- | --- | --- | --- |
| `workers` / `--workers` | Wie viele **Objekte** unterwegs sind. Auch die HEAD-Parallelität von `diff` | Vor allem kleine Objekte, und CPU, Platte und Proxy haben noch Luft | 503, Timeouts, eine volle Quellplatte, oder ein einzelnes Objekt wird untersucht (`--workers 1`) |
| `multipart_concurrency` / `--multipart-concurrency` | Wie viele **Parts eines großen Objekts** gleichzeitig hochgeladen werden | Ein einzelnes Objekt ist groß und dieser Strom kann mehr von der Leitung nutzen | Große Objekte füllen die Leitung schon, oder mehrere zusammen füllen den Proxy |
| `rate_limit` / `--rate-limit` | Wie viele **Quell-Bytes pro Sekunde** alle diese Worker zusammen lesen dürfen | Quelle und Netz haben noch Luft | Produktion nutzt den Quell-Cluster oder dieselbe NIC. Bei `20M` beginnen, dann `30M` oder `50M` |

`scan` ignoriert `workers` und `rate_limit`. Das Listing geht Seite für Seite, weil jede Seite das Continuation-Token der vorigen braucht und die Zeilen in einer SQLite-Datei landen. Ein langsamer Scan wird durch `--workers` nicht schneller. `list`, `diff` und `status` lesen keine Objektkörper, die Bytegrenze gilt für sie nicht. `diff` nutzt `--workers` für HEAD.

```text
workers = 4 Objekte gleichzeitig
  Objekt A (klein)  ── ein GET + PUT
  Objekt B (klein)  ── ein GET + PUT
  Objekt C (groß)   ── multipart_concurrency Range-GETs + UploadPart
  Objekt D (groß)   ── dasselbe
```

Die Spitze gleichzeitiger Anfragen an einen Cluster liegt bei etwa `workers × multipart_concurrency`. Ein kleines Objekt nutzt eine Anfrage. Ein Objekt an der Multipart-Schwelle multipliziert mit der Part-Nebenläufigkeit. Der Default `16 × 4` ist 64. `maxconn` von HAProxy oder Nginx und die Platten beider Cluster müssen dieses Produkt tragen.

Der HTTP-Client hebt die Idle-Verbindungen pro Host auf dieses Produkt an (gedeckelt bei 1024), statt des Go-Defaults von zwei Idle-Verbindungen pro Host. Das ist Wiederverwendung. Begrenzt wird die laufende Arbeit vom Worker-Pool und vom Part-Semaphor. Die Warteschlange hält höchstens 2048 Objektbeschreibungen, ein großes `--workers` lädt also nicht den ganzen Bucket in den Speicher.

Ausgangspunkte:

| Last | Vorschlag |
| --- | --- |
| Viele kleine Objekte (Kilobytes bis wenige Megabytes), die Leitung hat Luft | `workers` 16–64, `multipart_concurrency` bei 4 lassen |
| Gemischte Größen | `workers` 8–32, `multipart_concurrency` 4–8 |
| Wenige sehr große Objekte | `workers` 2–8, `multipart_concurrency` 4–16 |
| Ein Objekt scheitert und das Log ist schwer zu lesen | `--workers 1` |
| Proxy liefert 503, Timeouts, oder die Quellplatte ist voll | Zuerst `workers` senken. Sind die langsamen Objekte groß, auch `multipart_concurrency` |
| Bandbreite für die Produktion lassen | `--rate-limit 20M`, dann `30M` oder `50M`, wenn die Quelle gesund aussieht. `0` hebt das Limit auf |
| Schneller bei kleinen Objekten | `workers` erhöhen |
| Schneller bei einem großen Objekt | `multipart_concurrency` erhöhen oder `multipart_part_size` senken (Bytes, Minimum 5 MiB) |

Während einer Kopie erscheint etwa alle 10 Sekunden eine Fortschrittszeile: processed, copied, conflicts, skipped, failed und bytes.

Jedes kleine Objekt im Flug hat eine Temp-Datei in `spool_dir`, bis etwa `multipart_threshold` Bytes, weil `PutObject` einen seekbaren Body braucht. Ist `/tmp` eine Speicherplatte, `spool_dir` auf eine echte Platte legen. Mehr Worker bedeuten mehr dieser Dateien gleichzeitig.

Eine Datenbankdatei, ein Prozess. SQLite öffnet im WAL-Modus mit einer Verbindung im Prozess, Statusupdates bleiben serialisiert, Objektbytes bleiben im Worker-Pool. Ein zweiter Prozess auf derselben Datei meldet weiterhin `database is locked`.

<a id="rate-limit"></a>

## Ratenlimit

`rate_limit` ist ein gemeinsamer Token-Bucket für jeden Kopier-Worker, einschließlich der Parts eines großen Objekts. Gezählt werden Bytes, die aus Quell-Objektkörpern gelesen werden. List, HEAD und die Konfliktprüfung vor dem Schreiben liegen außerhalb. Der Upload nach einem abgeschlossenen Lesen wird nicht ein zweites Mal gezählt.

Ein kleines Objekt wird an der Grenze von der Quelle gelesen und dann auf das Ziel geschrieben. Lesevorgänge an der Quelle bleiben in der Grenze. Das folgende Schreiben kann kurz schneller sein. Teilt sich der Migrationsrechner eine NIC mit der Produktion, verbraucht dieses Schreiben zusätzlich Bandbreite. Soll die NIC insgesamt bei etwa 40 MB/s bleiben, bei `20M` beginnen.

Die Grenze verlangsamt Kopien. Sie überschreibt nicht und löscht nicht. Ein Objekt, das schon zum Inventar passt, wird weiter übersprungen. Das erste Ctrl-C bricht ein Lesen ab, das auf Tokens wartet. Das unfertige Objekt bleibt für das nächste `migrate` in SQLite.

Die Grenze senkt nicht die Zahl der Anfragen im Flug. Ein Proxy, der 503 liefert, ist weiter ein Nebenläufigkeitsproblem: zuerst `workers` senken, dann `multipart_concurrency`.

### Einheiten

Suffixe sind unabhängig von der Großschreibung. Ein abschließendes `/s` wird akzeptiert und ignoriert. Eine nackte Zahl ist Bytes pro Sekunde. `1.5M` sind 1.500.000 Bytes/s.

| Geschrieben als | Bytes pro Sekunde |
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
| `0`, `0M` oder weglassen | unbegrenzt |

`M` ist dezimal. `Mi` ist binär. `20M` liegt etwa 5 % unter `20MiB` (20000000 gegenüber 20971520). Ein negativer Wert oder ein unbekanntes Suffix scheitert beim Start.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # bewusst unbegrenzt
```

<a id="resume-and-object-state"></a>

## Fortsetzen und Objektstatus

Das erste Ctrl-C oder `SIGTERM` bricht den Worker-Pool ab. Noch nicht begonnene Objekte bleiben `pending`. Ein laufender Multipart-Upload wird abgebrochen, das gibt nur diesen Upload frei und löscht kein committetes Objekt. Bereits nach SQLite geschriebener Zustand bleibt. Ein zweites Signal beendet sofort mit Code 130.

Wenn `PutObject` auf dem Ziel schon erfolgreich war und die Zeile noch nicht `done` ist, bleibt die Zeile `failed` (oder `pending`), obwohl das Objekt auf dem Ziel liegt. Das nächste `migrate` prüft das Ziel mit `HeadObject`. Passen Größe und ein vergleichbares ETag, wird die Zeile `done`. Weicht die Größe ab oder ein vergleichbares ETag, wird die Zeile `conflict`, und das Objekt wird weiter nicht überschrieben.

Eine abgebrochene Anfrage, eine Deadline und `request canceled` sind `failed`, auch wenn der Fehlertext `NotFound` oder `StatusCode: 404` enthält. Ein Multipart-Abbruch meldet oft `NoSuchUpload`. Das ist kein fehlendes Quellobjekt. `skipped` heißt: Die Quellanfrage ist zu Ende gelaufen, und das Objekt war wirklich weg.

```bash
./bin/migration migrate -c config.yaml
```

`migrate` wiederholt nur `pending` und `failed`.

| Status | Bedeutung | Ein späterer Scan |
| --- | --- | --- |
| `pending` | Wartet auf die Kopie | Aktualisiert Größe und ETag |
| `failed` | Der letzte Versuch ist gescheitert. `migrate` wiederholt ihn. Die Meldung steht in `last_error` | Setzt die Zeile auf `pending` zurück |
| `skipped` | Die Quellanfrage endete mit 404 / `NoSuchKey` | Zurück auf `pending`, wenn der Schlüssel wieder gelistet wird |
| `done` | Kopiert und geprüft, oder das Ziel passte schon zum Inventar | Unverändert |
| `conflict` | Das Ziel hat den Schlüssel, und er passt nicht zum Inventar. Nicht überschrieben | Unverändert |

**Vergleichsregel.** Die Größe muss der inventarisierten Größe entsprechen. ETags werden nur verglichen, wenn beide Seiten eines haben und keines ein Multipart-ETag ist (ein Wert mit `-`, von der Form `hash-N`). Multipart-ETags hängen von den Part-Grenzen ab und werden nicht verglichen; geprüft wird die Größe. Eine andere Größe oder ein abweichendes vergleichbares ETag ist ein Konflikt.

Jeder Konflikt ist eine TSV-Zeile im Konfliktlog, Zeitstempel in RFC3339 UTC:

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

Ein konfigurierter Bucket nutzt `db_path` und `conflict_log` wie geschrieben. Der Modus für alle Buckets leitet Nachbardateien ab:

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

Eine Datenbankdatei ist an einen Bucket gebunden. Sie auf einen anderen Bucket zu richten scheitert mit `db_path is bound to bucket "…"`.

## Das Ziel mit diff prüfen

`diff` listet den Quell-Bucket live und macht `HEAD` für jeden Schlüssel auf dem Ziel. Es liest nicht das SQLite-Inventar der Migration und lädt nichts hoch.

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

Die zweite Summenzeile wiederholt die Zähler auf Chinesisch, weil der Befehl sie so druckt. Höchstens 50 Einzelfälle werden gezeigt; der Rest steht in einer Schlusszeile „and N more“.

| Ergebnis | Bedeutung |
| --- | --- |
| `missing` | Auf der Quelle, nicht auf dem Ziel. `migrate` oder `run` erneut |
| `size` / `etag` | Gleicher Schlüssel, Inhalt passt nicht. Oft ein Live-Schreiben auf dem Ziel. Dieses Werkzeug überschreibt ihn nicht |
| `extra` | Nur auf dem Ziel. Erscheint mit `--extra`. Meist ein Schreiben, das schon auf dem neuen Cluster liegt. Kein Fehler |

Exit-Code 1, wenn ein Schlüssel fehlt oder in Größe oder ETag abweicht. `extra_on_target` lässt den Befehl nicht scheitern. Bei mehreren Buckets scheitert der Bucket mit der Lücke, die übrigen laufen weiter, und der Prozess endet mit 1, wenn irgendein Bucket gescheitert ist.

## Sicherheit

| Garantie | Wie |
| --- | --- |
| Kein Überschreiben eines Zielobjekts | `HeadObject` vor jedem Schreiben. Eine Übereinstimmung ist `done`. Eine Abweichung ist `conflict.log` und ein Überspringen |
| Kein Löschen | Kein `DeleteObject`. Ein gescheiterter Multipart-Upload wird nur abgebrochen |
| Kein Bucket-Wechsel | `HeadBucket`, eine Bucket-Namen-Prüfung auf jeder Listenseite, und ein Bucket-Name in der SQLite-Datei |
| Löschungen auf der Quelle während des Laufs | Ein abgeschlossenes Quell-404 ist `skipped`. Abbruch und Timeout sind `failed` und werden wiederholt |
| Fortsetzen nach Absturz und Abbruch | Der Zustand je Objekt liegt in einer SQLite-WAL-Datei. Das erste Signal bricht Worker ab; das nächste `migrate` macht weiter |
| Begrenzter Speicher | Kleine Objekte gehen auf die Platte. Große Objekte werden partweise gelesen. Die Job-Queue ist gedeckelt |
| Prüfung nach der Kopie | Immer die Größe. Zusätzlich das ETag bei einem Single-Part-Objekt, wenn beide ETags vergleichbar sind |

Außerhalb: Versionshistorie, Delete-Marker, Object Lock, Bucket-Policies, Lifecycle-Regeln, Zwei-Wege-Sync und serverseitiges `CopyObject` (die Cluster sind getrennt, die Bytes laufen durch diesen Prozess).

## Fehlersuche

| Was zu sehen ist | Was zu tun ist |
| --- | --- |
| `read config … copy config.example.yaml first` | Der Pfad von `-c` fehlt. `config.example.yaml` kopieren |
| `old.endpoint is required` | `old.endpoint` oder `OLD_ENDPOINT` setzen. Die Zielseite meldet ebenso `new.endpoint is required`. Schlüssel melden `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`, `WORKERS` oder YAML-`workers` ist unter 1. YAML-`0` heißt „Default 16“. `0` auf der CLI oder in der Umgebung ist ein Fehler |
| `multipart_concurrency must be >= 1` | Dieselbe Regel für die Part-Nebenläufigkeit. YAML-`0` heißt „Default 4“ |
| `env WORKERS="…" is not an integer` | `WORKERS` muss eine nackte Ganzzahl sein. Bandbreite gehört nach `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | Nicht-Schluss-Parts müssen mindestens 5 MiB sein. Der Default 16 MiB ist gültig. Der Wert wird nicht automatisch angehoben |
| `unknown size suffix` oder `rate limit` | `rate_limit`, `RATE_LIMIT` oder `--rate-limit` wurde nicht erkannt. Dezimal ist `20M`. Binär ist `20Mi` oder `20MiB` |
| `head bucket "…"` | Der Bucket fehlt, der Endpunkt stimmt nicht, oder der Schlüssel darf nicht lesen. Ein Anlegefehler auf dem Ziel lautet `create bucket "…" on target (need s3:CreateBucket)` |
| Die Quelle ist bei `20M` noch stark belastet | `20M` sind 20000000 Bytes/s an Quell-Körpern. Der Upload liegt außerhalb dieser Grenze. Auf einer geteilten NIC die Zahl senken |
| `scan` oder `diff` wurde nicht langsamer | Erwartet. Die Grenze gilt, während `run` / `migrate` Objektkörper lesen |
| Der Scan bleibt langsam, obwohl `--workers` höher ist | Erwartet. `scan` ist einsträngig. Nebenläufigkeit beginnt bei der Kopie und beim HEAD von `diff` |
| Viele 503 oder Timeouts | `workers` senken. Bei großen Objekten auch `multipart_concurrency`, damit `workers × multipart_concurrency` in `maxconn` des Proxys passt |
| Viele Ergebnisse `etag mismatch` | SSE oder Kompression an der Quelle kann das ETag ändern. `multipart_threshold` senken und den Multipart-Pfad nehmen (Größe wird geprüft, ETag nicht), oder die Objekte von Hand stichproben |
| `db_path is bound to bucket "…"` | Diese Datei gehört schon zu einem anderen Bucket. Einen anderen `db_path` verwenden |
| `database is locked` | Zwei Prozesse schreiben dieselbe Datenbank. Ein Prozess pro Datei |
| Die Platte füllt sich mit Temp-Dateien | `spool_dir` auf eine große Platte legen oder `workers` senken |
| Ctrl-C kehrt nicht zurück | Auf die aktuelle Anfrage warten oder noch einmal drücken (Exit 130) |
| `diff` meldet weiter `missing` | `migrate` erneut ausführen und `last_error` der `failed`-Zeilen lesen |

## Exit-Codes

| Code | Bedeutung |
| --- | --- |
| 0 | Erfolg. Bei einem einzelnen Bucket endet auch das erste Ctrl-C in der Kopierphase (`migrate` oder die Kopierhälfte von `run`) mit 0. Unfertige Objekte bleiben in SQLite |
| 1 | Fehlerhafte Konfiguration, ein Fehler der Gegenstelle, ein gescheiterter Bucket, oder `diff` hat einen fehlenden oder abweichenden Schlüssel gefunden. Auch das erste Ctrl-C während `scan` und eine Unterbrechung, während mehr als ein Bucket im Lauf ist |
| 130 | Ein zweites `SIGINT` oder `SIGTERM`. Der Prozess endet sofort |

Fehler des Befehls erscheinen auf stderr als `error: …`.

## Lizenz

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
