# migration

<p align="center">
  <a href="https://github.com/rustfs/migration/blob/main/README.md">English</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ZH.md">简体中文</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_DE.md">Deutsch</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_ES.md">Español</a> |
  français |
  <a href="https://github.com/rustfs/migration/blob/main/README_JA.md">日本語</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_KO.md">한국어</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_PT.md">Portuguese</a> |
  <a href="https://github.com/rustfs/migration/blob/main/README_RU.md">Русский</a>
</p>

`migration` copie des objets dans un seul sens entre deux clusters compatibles S3. Il vise RustFS et MinIO auto-hébergés, ainsi que tout autre point de terminaison qui parle l'API S3. Les requêtes utilisent toujours l'adressage path-style, habituel sur ces clusters.

On peut le lancer à côté du trafic en production. Trois règles valent pour chaque commande qui écrit :

- **Jamais d'écrasement.** Avant chaque écriture, l'outil appelle `HeadObject` sur la cible. Une correspondance est marquée terminée. Un objet différent est seulement journalisé et laissé tel quel.
- **Jamais de suppression.** Il n'y a pas d'appel `DeleteObject`. Abandonner un envoi multipart ne retire que cet envoi inachevé, pas un objet déjà validé.
- **Reprise possible.** Chaque objet a une ligne dans une base SQLite locale. Arrêter puis relancer ne refait pas le travail déjà terminé.

Disposition courante : l'ancien cluster garde l'historique, le nouveau reçoit toutes les écritures, et l'application lit déjà les deux. Cet outil complète les clés que la cible n'a pas encore. Il ne remplace pas un objet plus récent sur la cible par des octets plus anciens venus de la source. Les conflits sur une même clé sont attendus dans cette disposition. Lire `conflict.log` et les traiter à la main.

La copie emporte l'objet courant, ainsi que `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` et les métadonnées utilisateur `x-amz-meta-*`. L'historique des versions, les marqueurs de suppression, Object Lock, les politiques de bucket et les règles de cycle de vie restent en place.

Dans le fichier de configuration, la source est `old` et la cible est `new`.

## Documentation

| Guide | Contenu |
| --- | --- |
| Ce fichier | Installation, configuration, commandes, réglage et sécurité en français |
| [README.md](README.md) | English, même structure |
| [README_ZH.md](README_ZH.md) | 简体中文 |
| [README_DE.md](README_DE.md) | Deutsch |
| [README_JA.md](README_JA.md) | 日本語 |
| [README_ES.md](README_ES.md) | Español |
| [README_KO.md](README_KO.md) | 한국어 |
| [README_PT.md](README_PT.md) | Portuguese |
| [README_RU.md](README_RU.md) | Русский |
| [docs/USAGE.md](docs/USAGE.md) | Court compagnon en anglais |
| [docs/使用手册.md](docs/使用手册.md) | Manuel chinois plus détaillé |
| [docs/DESIGN.md](docs/DESIGN.md) | Architecture et les contraintes que l'outil ne relâche pas |

`migration --help` et `migration <commande> --help` affichent l'aide en anglais et en chinois.

## Démarrage rapide

Go 1.22 ou plus récent. SQLite est en Go pur : la compilation n'a pas besoin de CGO.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
cp config.example.yaml config.yaml   # points de terminaison et clés, source et cible
CGO_ENABLED=0 go build -o bin/migration .
# ou : make build

./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Un `bucket:` vide migre chaque bucket de la source. `bucket: photos`, ou `--bucket photos`, n'en retient qu'un.

`20M` est un mégaoctet décimal par seconde : 20×1000×1000 octets/s. Avant de l'augmenter sur un réseau partagé, lire [Limite de débit](#rate-limit).

## Installation

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
```

`make build` lance la même commande. Le résultat est le binaire statique `bin/migration`. `make test` lance `go test ./...`.

Protéger `config.yaml` (`chmod 600` est un défaut raisonnable). Le processus journalise les points de terminaison et les clés d'objet. Il ne journalise pas les secrets.

## Configuration

```bash
cp config.example.yaml config.yaml
```

Le minimum est un point de terminaison, une access key et une secret key de chaque côté. La clé cible a besoin de `s3:CreateBucket` quand le bucket n'existe pas encore. Un bucket cible déjà présent n'est pas modifié.

```yaml
old:                              # source, lecture seule
  endpoint: "http://10.0.0.1:9000"
  access_key: "OLD_AK"
  secret_key: "OLD_SK"
  region: "us-east-1"
new:                              # cible, lecture et écriture
  endpoint: "http://haproxy.new.local:9000"
  access_key: "NEW_AK"
  secret_key: "NEW_SK"
  region: "us-east-1"

bucket: ""                        # vide = chaque bucket de la source
db_path: "./migration.db"
conflict_log: "./conflict.log"
spool_dir: ""                     # répertoire temporaire des petits objets ; défaut = répertoire de la base

workers: 16                       # objets à la fois ; 0 ou omis = 16
multipart_threshold: 52428800     # >= 50 MiB passe en envoi multipart
multipart_part_size: 16777216     # 16 MiB ; doit être >= 5 MiB (5242880)
multipart_concurrency: 4          # parts d'un gros objet ; 0 ou omis = 4
rate_limit: 0                     # plafond de lecture agrégé sur la source ; 0 ou omis = illimité
```

`list` et `scan` ont besoin du point de terminaison et des clés de la source. `run`, `migrate` et `diff` ont besoin des deux côtés.

### Priorité

La priorité la plus haute l'emporte :

1. Ligne de commande : `--bucket`, `--workers`, `--multipart-concurrency`, `--rate-limit`
2. Variables d'environnement
3. YAML
4. Valeurs par défaut intégrées

| Réglage | Défaut si omis |
| --- | --- |
| `workers` | 16 |
| `multipart_concurrency` | 4 |
| `multipart_threshold` | 50 MiB (52428800) |
| `multipart_part_size` | 16 MiB (16777216) |
| `rate_limit` | illimité |
| `db_path` | `./migration.db` |
| `conflict_log` | `./conflict.log` |
| `spool_dir` | répertoire de `db_path` |
| `region` | `us-east-1` |

Exemple : YAML `workers: 8`, environnement `WORKERS=32` et `--workers 4` ensemble utilisent **4**.

### Ce que vaut zéro

`0` n'a pas le même sens sur chaque champ.

| Champ | YAML `0` ou omis | Environnement `0` | CLI `0` |
| --- | --- | --- | --- |
| `workers` | défaut 16 | erreur | erreur (`--workers 0`) |
| `multipart_concurrency` | défaut 4 | erreur | erreur |
| `multipart_threshold` | défaut 50 MiB | pas de variable | pas d'option |
| `multipart_part_size` | défaut 16 MiB | pas de variable | pas d'option |
| `rate_limit` | illimité | illimité (`RATE_LIMIT=0`) | illimité (`--rate-limit 0`) |

Une concurrence ou une taille négative fait échouer le démarrage. `WORKERS` et `MULTIPART_CONCURRENCY` doivent être des entiers simples. Les suffixes de taille vont seulement sur `RATE_LIMIT` : `RATE_LIMIT=20M` est valide, `WORKERS=20M` ne l'est pas.

`multipart_part_size` doit valoir au moins 5 MiB (5242880). S3 refuse une part non finale plus petite avec `EntityTooSmall`. Une valeur plus petite échoue au chargement de la configuration. Elle n'est pas relevée silencieusement. Si un objet dépasserait 10 000 parts, l'outil agrandit la taille de part de cet objet pour que l'envoi tienne.

Un nom de bucket ne doit contenir ni espace ni `/`.

### Variables d'environnement

Une variable définie remplace le YAML. Une variable absente laisse le fichier tel quel.

| Variable | Remplace |
| --- | --- |
| `OLD_ENDPOINT` `OLD_AK` `OLD_SK` `OLD_REGION` | point de terminaison source |
| `NEW_ENDPOINT` `NEW_AK` `NEW_SK` `NEW_REGION` | point de terminaison cible |
| `REGION` | chaque côté dont la région est encore vide après les variables spécifiques |
| `BUCKET` | `bucket` |
| `DB_PATH` `CONFLICT_LOG` `SPOOL_DIR` | chemins locaux |
| `WORKERS` `MULTIPART_CONCURRENCY` | concurrence (entier ≥ 1) |
| `RATE_LIMIT` | plafond de lecture agrégé sur la source (`20M`, `20MiB`, `0`, …) |

## Commandes

```bash
./bin/migration --help
./bin/migration run --help
./bin/migration diff --help
```

| Commande | Rôle | Écrit sur la cible | Concurrence et plafond |
| --- | --- | --- | --- |
| `migration list -c config.yaml` | Lister tous les buckets de la source | Non | Aucune |
| `migration run -c config.yaml` | **Recommandé.** Inventorier, puis copier | Oui, sans écraser | L'inventaire est monothread. La copie utilise `workers`, `multipart_concurrency` et `rate_limit` |
| `migration scan -c config.yaml` | Inventorier la source dans SQLite | Non | Monothread. Pas de limite de débit |
| `migration migrate -c config.yaml` | Copier seulement les lignes SQLite `pending` ou `failed` | Oui, sans écraser | `workers`, `multipart_concurrency` et `rate_limit` |
| `migration status -c config.yaml` | Afficher les compteurs locaux | Non | Aucune. Aucun appel S3 |
| `migration diff -c config.yaml` | Lister la source et faire un `HEAD` de chaque clé sur la cible | Non | `workers` pour les HEAD parallèles. Pas de limite de débit |
| `migration diff --extra -c config.yaml` | Idem, et lister les clés présentes seulement sur la cible | Non | `workers` |

Options communes à toutes les commandes :

| Option | Sens |
| --- | --- |
| `-c`, `--config` | Fichier de configuration. Défaut `config.yaml` |
| `--bucket NAME` | Fixer un bucket. Remplace le YAML et `BUCKET` |
| `--workers N` | Objets copiés en même temps pour `run` / `migrate`, et HEAD parallèles pour `diff`. Doit être ≥ 1 |
| `--multipart-concurrency N` | Parts envoyées en même temps dans un gros objet. Doit être ≥ 1. Ignoré par `list`, `scan`, `status` et `diff` |
| `--rate-limit SIZE` | Plafond de lecture agrégé des corps d'objet dans `run` / `migrate`. `0` est illimité |
| `--version` | Afficher la version |
| `--help` | Aide bilingue |

`diff` accepte aussi `--extra`.

`status` affiche `BUCKET`, `TOTAL`, `PENDING`, `DONE`, `CONFLICT`, `SKIPPED` et `FAILED`. Sans bucket fixé, il lit les `migration-*.db` à côté de `db_path`, et la base configurée elle-même si ce fichier est déjà lié à un bucket. S'il n'y a encore rien, il le dit et sort avec 0. Lancer d'abord `scan` ou `run`.

## Parcours

### Migrer tous les buckets

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Pour chaque bucket, `run` fait ceci :

1. `HeadBucket` sur la source.
2. Créer le bucket sur la cible s'il manque. Un bucket existant n'est pas modifié.
3. `ListObjectsV2` de la source vers SQLite. Cette étape est monothread.
4. Démarrer `workers` goroutines de copie. Chaque objet est d'abord vérifié par `HeadObject` sur la cible. Une clé absente est lue sur la source et écrite sur la cible. Ces lectures partagent `rate_limit`. Une clé qui correspond déjà à l'inventaire est marquée `done` et n'est pas envoyée. Une clé présente mais différente est ajoutée au journal de conflits et n'est pas écrasée.
5. Un objet à partir de `multipart_threshold` envoie ses parts avec `multipart_concurrency`. Ces parts partagent le même plafond d'octets.
6. Vérifier la taille sur la cible. Un objet en une seule part est aussi vérifié par ETag quand les ETag sont comparables. Voir [État des objets](#resume-and-object-state).

Les buckets sont copiés l'un après l'autre : `rate_limit` est donc le plafond de tout le processus.

### Un seul bucket

```bash
./bin/migration run -c config.yaml \
  --bucket photos --workers 8 --multipart-concurrency 8 --rate-limit 30M
```

### Objets écrits plus tard sur la source

`ListObjectsV2` ne peut pas filtrer par date de modification côté serveur. Le rattrapage est un nouveau listage complet. Les nouvelles clés deviennent `pending`. Les lignes `failed` ou `skipped` reviennent à `pending` quand elles sont listées de nouveau. `done` et `conflict` restent en place : un second passage ne les renvoie pas et n'écrase pas la cible.

```bash
./bin/migration scan    -c config.yaml
./bin/migration migrate -c config.yaml --workers 16
./bin/migration diff    -c config.yaml
```

Relancer `run` réunit ces deux étapes dans une commande.

### Inventorier d'abord

```bash
./bin/migration scan   -c config.yaml
./bin/migration status -c config.yaml    # compteurs d'objets, aucun appel S3
./bin/migration migrate -c config.yaml --workers 16
```

## Concurrence

Trois réglages, trois rôles :

| Réglage | Ce qu'il limite | L'augmenter quand | Le baisser quand |
| --- | --- | --- | --- |
| `workers` / `--workers` | Combien d'**objets** sont en vol. Aussi le parallélisme HEAD de `diff` | Surtout de petits objets, et le CPU, le disque et le proxy ont encore de la marge | Des 503, des timeouts, un disque source saturé, ou un objet isolé à examiner (`--workers 1`) |
| `multipart_concurrency` / `--multipart-concurrency` | Combien de **parts d'un gros objet** partent ensemble | Un seul objet est gros et ce flux peut encore utiliser le lien | Les gros objets remplissent déjà le lien, ou plusieurs ensemble remplissent le proxy |
| `rate_limit` / `--rate-limit` | Combien d'**octets source par seconde** tous ces workers peuvent lire, ensemble | La source et le réseau ont encore de la marge | La production utilise le cluster source ou la même carte réseau. Commencer à `20M`, puis essayer `30M` ou `50M` |

`scan` ignore `workers` et `rate_limit`. Le listage avance page par page : chaque page a besoin du jeton de continuation de la précédente, et les lignes vont dans un seul fichier SQLite. Un scan lent ne s'accélère pas avec `--workers`. `list`, `diff` et `status` ne lisent pas les corps d'objet, le plafond d'octets ne s'applique pas. `diff` utilise `--workers` pour les HEAD.

```text
workers = 4 objets à la fois
  objet A (petit)  ── un GET + PUT
  objet B (petit)  ── un GET + PUT
  objet C (gros)   ── multipart_concurrency GET par plage + UploadPart
  objet D (gros)   ── la même chose
```

Le pic de requêtes en vol vers un cluster est d'environ `workers × multipart_concurrency`. Un petit objet utilise une requête. Un objet au seuil multipart multiplie par la concurrence des parts. Le défaut `16 × 4` vaut 64. Le `maxconn` de HAProxy ou de Nginx, et les disques des deux clusters, doivent porter ce produit.

Le client HTTP relève les connexions inactives par hôte jusqu'à ce produit (plafonné à 1024), au lieu des deux connexions inactives par hôte du défaut Go. C'est de la réutilisation. Le travail en vol est borné par le pool de workers et le sémaphore des parts. La file d'attente tient au plus 2048 descripteurs d'objet : un grand `--workers` ne charge pas tout le bucket en mémoire.

Points de départ :

| Charge | Suggestion |
| --- | --- |
| Beaucoup de petits objets (kilo-octets à quelques mégaoctets), le lien a de la marge | `workers` 16–64, laisser `multipart_concurrency` à 4 |
| Tailles mélangées | `workers` 8–32, `multipart_concurrency` 4–8 |
| Quelques très gros objets | `workers` 2–8, `multipart_concurrency` 4–16 |
| Un objet échoue et le journal est difficile à lire | `--workers 1` |
| Le proxy renvoie 503, des timeouts, ou le disque source est plein | Baisser d'abord `workers`. Si les objets lents sont gros, baisser aussi `multipart_concurrency` |
| Garder de la bande passante pour la production | `--rate-limit 20M`, puis `30M` ou `50M` quand la source semble saine. `0` retire le plafond |
| Aller plus vite sur les petits objets | Augmenter `workers` |
| Aller plus vite sur un gros objet | Augmenter `multipart_concurrency`, ou baisser `multipart_part_size` (octets, minimum 5 MiB) |

Pendant une copie, une ligne de progression est journalisée environ toutes les 10 secondes : processed, copied, conflicts, skipped, failed et bytes.

Chaque petit objet en vol a un fichier temporaire dans `spool_dir`, jusqu'à environ `multipart_threshold` octets, parce que `PutObject` a besoin d'un corps seekable. Si `/tmp` est un disque mémoire, pointer `spool_dir` vers un vrai disque. Plus de workers, plus de ces fichiers en même temps.

Un processus par fichier de base. SQLite s'ouvre en mode WAL avec une seule connexion dans le processus : les mises à jour d'état restent sérialisées, les octets d'objet restent sur le pool de workers. Un second processus sur le même fichier signale encore `database is locked`.

<a id="rate-limit"></a>

## Limite de débit

`rate_limit` est un seul seau de jetons partagé par chaque worker de copie, y compris les parts d'un gros objet. Il compte les octets lus dans les corps d'objet de la source. List, HEAD et le contrôle de conflit avant écriture sont hors plafond. L'envoi qui suit une lecture terminée n'est pas compté une seconde fois.

Un petit objet est lu sur la source au plafond, puis écrit sur la cible. Les lectures source restent dans le plafond. L'écriture qui suit peut être brièvement plus rapide. Si l'hôte de migration partage une carte réseau avec la production, cette écriture consomme encore de la bande passante. Pour garder l'usage total de la carte autour de 40 Mo/s, commencer à `20M`.

Le plafond ralentit les copies. Il n'écrase pas et il ne supprime pas. Un objet qui correspond déjà à l'inventaire est toujours sauté. Le premier Ctrl-C annule une lecture qui attend des jetons. L'objet inachevé reste dans SQLite pour le prochain `migrate`.

Le plafond ne réduit pas le nombre de requêtes en vol. Un proxy qui renvoie 503 reste un problème de concurrence : baisser `workers`, puis `multipart_concurrency`.

### Unités

Les suffixes ignorent la casse. Un `/s` final est accepté et ignoré. Un nombre nu est en octets par seconde. `1.5M` vaut 1 500 000 octets/s.

| Écrit comme | Octets par seconde |
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
| `0`, `0M`, ou omis | illimité |

`M` est décimal. `Mi` est binaire. `20M` est environ 5 % sous `20MiB` (20000000 contre 20971520). Une valeur négative ou un suffixe inconnu échoue au démarrage.

```yaml
rate_limit: 20M
```

```bash
RATE_LIMIT=30M ./bin/migration run -c config.yaml
./bin/migration migrate -c config.yaml --rate-limit 50M
./bin/migration run -c config.yaml --rate-limit 0    # illimité, volontairement
```

<a id="resume-and-object-state"></a>

## Reprise et état des objets

Le premier Ctrl-C ou `SIGTERM` annule le pool de workers. Les objets pas encore commencés restent `pending`. Un envoi multipart en cours est abandonné : cela libère seulement cet envoi et ne supprime pas un objet validé. L'état déjà écrit dans SQLite est conservé. Un second signal quitte immédiatement avec le code 130.

Si le `PutObject` cible a réussi et que la ligne n'est pas encore marquée `done`, la ligne reste `failed` (ou `pending`) alors que l'objet est déjà sur la cible. Le `migrate` suivant vérifie la cible avec `HeadObject`. Quand la taille correspond et qu'un ETag comparable correspond, la ligne passe à `done`. Quand la taille diffère, ou qu'un ETag comparable diffère, la ligne passe à `conflict`, et l'objet n'est toujours pas écrasé.

Une requête annulée, une échéance et `request canceled` sont `failed`, même si le texte d'erreur contient aussi `NotFound` ou `StatusCode: 404`. Un abandon multipart signale souvent `NoSuchUpload`. Ce n'est pas un objet source manquant. `skipped` signifie que la requête source s'est terminée et que l'objet avait vraiment disparu.

```bash
./bin/migration migrate -c config.yaml
```

`migrate` ne reprend que `pending` et `failed`.

| État | Sens | Un scan ultérieur |
| --- | --- | --- |
| `pending` | En attente de copie | Rafraîchit la taille et l'ETag |
| `failed` | La dernière tentative a échoué. `migrate` la reprend. Le message est dans `last_error` | Ramène la ligne à `pending` |
| `skipped` | La requête source s'est terminée par 404 / `NoSuchKey` | Ramène la ligne à `pending` si la clé est listée de nouveau |
| `done` | Copié et vérifié, ou la cible correspondait déjà à l'inventaire | Inchangé |
| `conflict` | La cible a la clé et elle ne correspond pas à l'inventaire. Pas d'écrasement | Inchangé |

**Règle de correspondance.** La taille doit égaler la taille inventoriée. Les ETag ne sont comparés que si les deux côtés en ont un et qu'aucun n'est un ETag multipart (une valeur contenant `-`, de la forme `hash-N`). Les ETag multipart dépendent des frontières de parts : ils ne sont pas comparés, la taille est le contrôle. Une taille différente, ou un ETag comparable différent, est un conflit.

Chaque conflit est une ligne TSV dans le journal, horodatages RFC3339 UTC :

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

Un seul bucket configuré utilise `db_path` et `conflict_log` tels qu'écrits. Le mode tous les buckets dérive des fichiers voisins :

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

Un fichier de base est lié à un bucket. Le pointer vers un autre bucket échoue avec `db_path is bound to bucket "…"`.

## Vérifier la cible avec diff

`diff` liste le bucket source en direct et fait un `HEAD` de chaque clé sur la cible. Il ne lit pas l'inventaire SQLite de la migration et il n'envoie rien.

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

La deuxième ligne de synthèse répète les compteurs en chinois, parce que la commande l'affiche ainsi. Au plus 50 lignes de problème sont montrées ; le reste est compté dans une ligne finale « and N more ».

| Résultat | Sens |
| --- | --- |
| `missing` | Sur la source, pas sur la cible. Relancer `migrate` ou `run` |
| `size` / `etag` | Même clé, contenus différents. Souvent une écriture déjà faite sur la cible. Cet outil ne l'écrase pas |
| `extra` | Seulement sur la cible. Signalé avec `--extra`. En général une écriture déjà arrivée sur le nouveau cluster. Pas un échec |

Le code de sortie est 1 si une clé manque ou si la taille ou l'ETag ne correspond pas. `extra_on_target` ne fait pas échouer la commande. Avec plusieurs buckets, un écart fait échouer ce bucket et la commande continue avec les autres, puis sort avec 1 si un bucket a échoué.

## Sécurité

| Garantie | Moyen |
| --- | --- |
| Pas d'écrasement d'un objet cible | `HeadObject` avant chaque écriture. Une correspondance est `done`. Un écart va dans `conflict.log` et la clé est sautée |
| Pas de suppression | Pas de `DeleteObject`. Un envoi multipart en échec est seulement abandonné |
| Pas de mélange de buckets | `HeadBucket`, un contrôle du nom de bucket sur chaque page de liste, et un nom de bucket stocké dans le fichier SQLite |
| Suppressions côté source pendant le passage | Un 404 source terminé est `skipped`. Annulation et délai sont `failed` et seront repris |
| Reprise après plantage et annulation | L'état de chaque objet est dans un fichier SQLite WAL. Le premier signal annule les workers ; le `migrate` suivant continue |
| Mémoire bornée | Les petits objets passent par le disque. Les gros objets sont lus par part. La file de travaux est plafonnée |
| Contrôle après copie | Toujours la taille. L'ETag aussi pour un objet en une seule part quand les deux ETag sont comparables |

Hors périmètre : historique des versions, marqueurs de suppression, Object Lock, politiques de bucket, règles de cycle de vie, synchronisation bidirectionnelle, et `CopyObject` côté serveur (les clusters sont séparés, les octets passent par ce processus).

## Dépannage

| Ce que l'on voit | Quoi faire |
| --- | --- |
| `read config … copy config.example.yaml first` | Le chemin de `-c` manque. Copier `config.example.yaml` |
| `old.endpoint is required` | Renseigner `old.endpoint` ou `OLD_ENDPOINT`. Le côté cible signale de la même façon `new.endpoint is required`. Les clés signalent `access_key` / `secret_key` |
| `workers must be >= 1` | `--workers`, `WORKERS` ou le `workers` du YAML est inférieur à 1. Un `0` dans le YAML veut dire « défaut 16 ». Un `0` en ligne de commande ou dans l'environnement est une erreur |
| `multipart_concurrency must be >= 1` | Même règle pour la concurrence des parts. Un `0` dans le YAML veut dire « défaut 4 » |
| `env WORKERS="…" is not an integer` | `WORKERS` doit être un entier nu. Le débit va dans `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | Les parts non finales doivent faire au moins 5 MiB. Le défaut de 16 MiB est déjà valide. La valeur n'est pas relevée automatiquement |
| `unknown size suffix` ou `rate limit` | `rate_limit`, `RATE_LIMIT` ou `--rate-limit` n'a pas été reconnu. Le décimal est `20M`. Le binaire est `20Mi` ou `20MiB` |
| `head bucket "…"` | Le bucket manque, le point de terminaison est faux, ou la clé ne peut pas lire. Un échec de création sur la cible dit `create bucket "…" on target (need s3:CreateBucket)` |
| La source est encore chargée à `20M` | `20M` vaut 20000000 octets/s de corps lus sur la source. L'envoi est hors de ce plafond. Sur une carte partagée, baisser le nombre |
| `scan` ou `diff` n'a pas ralenti | Attendu. Le plafond s'applique pendant que `run` / `migrate` lisent les corps d'objet |
| Le scan reste lent après avoir augmenté `--workers` | Attendu. `scan` est monothread. La concurrence commence à la copie, et au HEAD de `diff` |
| Beaucoup de 503 ou de timeouts | Baisser `workers`. Pour les gros objets, baisser `multipart_concurrency` afin que `workers × multipart_concurrency` tienne dans le `maxconn` du proxy |
| Beaucoup de résultats `etag mismatch` | Un SSE ou une compression côté source peut changer l'ETag. Baisser `multipart_threshold` pour prendre le chemin multipart (la taille est vérifiée, l'ETag ne l'est pas), ou échantillonner les objets à la main |
| `db_path is bound to bucket "…"` | Ce fichier appartient déjà à un autre bucket. Utiliser un autre `db_path` |
| `database is locked` | Deux processus écrivent la même base. Un processus par fichier |
| Le disque se remplit de fichiers temporaires | Déplacer `spool_dir` vers un grand disque, ou baisser `workers` |
| Ctrl-C ne revient pas | Attendre la requête en cours, ou appuyer encore (sortie 130) |
| `diff` signale encore `missing` | Relancer `migrate` et lire `last_error` des lignes `failed` |

## Codes de sortie

| Code | Sens |
| --- | --- |
| 0 | Succès. Pour un seul bucket, le premier Ctrl-C pendant la copie (`migrate`, ou la moitié copie de `run`) sort aussi avec 0. Les objets inachevés restent dans SQLite |
| 1 | Configuration invalide, erreur distante, un bucket en échec, ou `diff` a trouvé une clé manquante ou différente. Aussi le premier Ctrl-C pendant `scan`, et une interruption alors que plus d'un bucket est dans le passage |
| 130 | Un second `SIGINT` ou `SIGTERM`. Le processus quitte immédiatement |

Les erreurs de commande s'affichent sur stderr sous la forme `error: …`.

## Licence

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
