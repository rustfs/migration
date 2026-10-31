# migration

[English](README.md) | [简体中文](README_ZH.md) | [Deutsch](README_DE.md) | [Español](README_ES.md) | **Français** | [日本語](README_JA.md) | [한국어](README_KO.md) | [Português](README_PT.md) | [Русский](README_RU.md)

`migration` copie la version courante des objets d’un cluster compatible S3 vers un autre. Vous pouvez ainsi transférer les données historiques d’un cluster RustFS ou MinIO pendant que les applications écrivent déjà sur le nouveau cluster. Dans la configuration, `old` désigne la source et `new` la destination. Les deux utilisent des URL S3 où le nom du bucket figure dans le chemin.

L’outil copie les clés manquantes, consigne les conflits dans `conflict.log` et enregistre sa progression dans une base SQLite locale. Il ne supprime aucun objet. La destination doit prendre en charge `If-None-Match: *` pour `PutObject` et `CompleteMultipartUpload`. Un objet créé par une application après la vérification initiale reste ainsi intact.

La copie conserve le contenu courant de l’objet, les en-têtes `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language` et `Expires` ainsi que les métadonnées utilisateur `x-amz-meta-*`. L’historique des versions, les marqueurs de suppression, Object Lock, les politiques de bucket et les règles de cycle de vie restent hors du périmètre de la migration.

## Documentation

[Aide-mémoire en anglais](docs/USAGE.md) · [Manuel d’exploitation en chinois](docs/使用手册.md) · [Architecture en anglais](docs/DESIGN.md)

## Démarrage rapide

Pour compiler le programme depuis le dépôt, utilisez Go 1.22 ou une version ultérieure. Après avoir copié le modèle, renseignez les adresses et les identifiants des deux clusters dans `config.yaml`.

```bash
git clone https://github.com/rustfs/migration.git
cd migration
CGO_ENABLED=0 go build -o bin/migration .
# ou : make build
cp config.example.yaml config.yaml   # points de terminaison et clés, source et cible
```

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml
```

Un `bucket:` vide migre chaque bucket de la source. `bucket: photos`, ou `--bucket photos`, n'en retient qu'un.

Utilisez `--bucket ""` pour sélectionner tous les buckets de la source, même si le fichier YAML ou `BUCKET` en désigne un seul.

Avec `--rate-limit 20M`, le débit de lecture cumulé sur la source est limité à 20 000 000 octets par seconde. Les autres unités sont détaillées dans la section [Limite de débit](#rate-limit).

## Installation

Dans [Build packages](https://github.com/rustfs/migration/actions/workflows/packages.yml), ouvrez une exécution réussie et choisissez votre plateforme sous **Artifacts**. Décompressez le téléchargement, vérifiez la somme SHA-256 de l’archive puis extrayez son contenu. Les archives Linux (amd64 et arm64) sont au format `.tar.gz`. Sous Windows (amd64), le fichier `.zip` contient `migration.exe`.

```bash
./migration --version
```

```powershell
.\migration.exe --version
```

Protéger `config.yaml` (`chmod 600` est un défaut raisonnable). Le processus journalise les points de terminaison et les clés d'objet. Il ne journalise pas les secrets.

## Configuration

```bash
cp config.example.yaml config.yaml
```

Indiquez l’adresse HTTP ou HTTPS et les identifiants de chaque cluster. Saisissez les identifiants dans les champs prévus à cet effet. L’URL ne doit contenir ni identifiants, ni paramètres de requête, ni fragment. La création d’un bucket sur la destination exige le droit `s3:CreateBucket`.

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

La valeur de `multipart_part_size` doit être comprise entre 5 Mio et 5 Gio (16 Mio par défaut). Tout objet de plus de 5 Gio est envoyé en plusieurs parties. Pour rester sous la limite de 10 000 parties, l’outil augmente leur taille si nécessaire. Le transfert échoue si la taille nécessaire dépasse 5 Gio par partie.

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

## Procédures

### Migrer tous les buckets

```bash
./bin/migration list   -c config.yaml
./bin/migration run    -c config.yaml --workers 16 --rate-limit 20M
./bin/migration status -c config.yaml
./bin/migration diff   -c config.yaml --workers 32
```

Pour chaque bucket, `run` fait ceci :

1. `HeadBucket` sur la source.
2. `ListObjectsV2` de la source vers SQLite. Cette étape est monothread.
3. Créer le bucket sur la cible s'il manque. Un bucket existant n'est pas modifié.
4. Lancer jusqu’à `workers` copies simultanées. Si une application crée la clé après la requête HEAD initiale, l’écriture conditionnelle protège cet objet. L’outil vérifie à nouveau la clé avant de lui attribuer l’état `done` ou `conflict`.
5. À partir de `multipart_threshold`, chaque objet est transféré en plusieurs parties, dont `multipart_concurrency` au maximum en parallèle. Toutes les lectures partagent la même limite de débit.
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

`workers` règle le nombre d’objets copiés simultanément. Pour les gros objets, `multipart_concurrency` fixe aussi le nombre de parties transférées en parallèle par objet. Avec les valeurs par défaut (16 objets et quatre parties), un cluster peut recevoir environ 64 requêtes simultanées. Tenez compte des disques, du proxy et de la mémoire disponible avant d’augmenter ces valeurs.

`scan` ignore `workers` et `rate_limit`. Le listage avance page par page : chaque page a besoin du jeton de continuation de la précédente, et les lignes vont dans un seul fichier SQLite. Un scan lent ne s'accélère pas avec `--workers`. `list`, `diff` et `status` ne lisent pas les corps d'objet, le plafond d'octets ne s'applique pas. `diff` utilise `--workers` pour les HEAD.

```text
workers = 4 objets à la fois
  objet A (petit)  ── un GET + PUT
  objet B (petit)  ── un GET + PUT
  objet C (gros)   ── multipart_concurrency GET par plage + UploadPart
  objet D (gros)   ── la même chose
```

Chaque partie en cours de transfert est mise en mémoire. La file d’attente contient au plus 2 048 descripteurs d’objet. Le client HTTP peut réutiliser ses connexions et en conserve jusqu’à 1 024 inactives par hôte.

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

Chaque petit objet en vol a un fichier temporaire dans `spool_dir`, jusqu'à environ `multipart_threshold` octets, parce que `PutObject` a besoin d'un flux repositionnable. Si `/tmp` est un disque mémoire, pointer `spool_dir` vers un vrai disque. Plus de workers, plus de ces fichiers en même temps.

Un processus par fichier de base. SQLite s'ouvre en mode WAL avec une seule connexion dans le processus : les mises à jour d'état restent sérialisées, les octets d'objet restent sur le pool de workers. Un second processus sur le même fichier signale encore `database is locked`.

<a id="rate-limit"></a>

## Limite de débit

`rate_limit` plafonne le débit cumulé des lectures sur la source, toutes copies et parties confondues. Les requêtes de liste, les lectures d’en-têtes et les envois vers la cible ne sont pas comptabilisés.

Les petits objets sont d’abord enregistrés sur le disque local, puis envoyés à la destination. Si les deux transferts empruntent la même interface réseau, prévoyez aussi de la bande passante pour l’envoi.

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

Si l’ETag de la source change entre l’inventaire et le transfert, la copie échoue. Actualisez l’inventaire avec `scan` avant de relancer `migrate`, ou relancez simplement `run`.

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

Si la clé existe déjà, l’objet ne passe à l’état `done` que si sa taille correspond et que les deux ETag sont renseignés et identiques. Sinon, il passe à l’état `conflict` et reste intact. Un même contenu peut avoir deux ETag différents si le découpage en parties diffère. En cas d’interruption juste après un transfert, vérifiez ces conflits manuellement.

Chaque conflit est une ligne TSV dans le journal, horodatages RFC3339 UTC :

```text
<object-key>	<source-LastModified>	<target-LastModified>
```

Les clés qui contiennent des caractères de contrôle ou commencent par un guillemet double sont écrites entre guillemets, avec les séquences d’échappement de Go.

Un seul bucket configuré utilise `db_path` et `conflict_log` tels qu'écrits. Le mode tous les buckets dérive des fichiers voisins :

- `migration-<bucket>.db`
- `conflict-<bucket>.log`

`migrate` nécessite un inventaire créé par `scan` ou `run`. La base est associée au bucket et aux points de terminaison. Avant de réutiliser une ancienne base, vérifiez qu’elle correspond aux bons clusters. La première analyse enregistre la source et la première migration enregistre la destination. Si l’analyse échoue ou est interrompue, relancez `scan` ou `run` avant d’utiliser `migrate`. Les anciennes bases ne conservent aucune trace de fin d’analyse. En cas de doute, relancez l’analyse.

## Vérifier la cible avec diff

`diff` vérifie les tailles et les ETag comparables sans télécharger les objets. Parmi les objets comptés dans `ok`, `size_only` indique ceux dont seule la taille a été vérifiée. Avec `--extra`, un index temporaire sert à repérer les clés présentes uniquement sur la destination. Si le bucket source est vide, `diff` vérifie que le bucket de destination existe et renvoie le code 1 s’il est absent.

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

La deuxième ligne de synthèse répète les compteurs en chinois, parce que la commande l'affiche ainsi. Au plus 50 lignes de problème sont montrées ; le reste est compté dans une ligne finale « and N more ».

| Résultat | Sens |
| --- | --- |
| `missing` | Clé présente sur la source et absente de la destination |
| `size` / `etag` | Même clé, contenus différents. Souvent une écriture déjà faite sur la cible. Cet outil ne l'écrase pas |
| `extra` | Seulement sur la cible. Signalé avec `--extra`. En général une écriture déjà arrivée sur le nouveau cluster. Pas un échec |

Le code de sortie est 1 si une clé manque ou si la taille ou l'ETag ne correspond pas. `extra_on_target` ne fait pas échouer la commande. Avec plusieurs buckets, un écart fait échouer ce bucket et la commande continue avec les autres, puis sort avec 1 si un bucket a échoué.

## Sécurité

| Garantie | Moyen |
| --- | --- |
| Pas d'écrasement d'un objet cible | `HeadObject` → `PutObject` / `CompleteMultipartUpload` (`If-None-Match: *`) |
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
| `env WORKERS="…" is not an integer` | `WORKERS` doit être un entier sans unité. Le débit va dans `RATE_LIMIT=20M` |
| `multipart_part_size must be >= 5242880` | Les parts non finales doivent faire au moins 5 MiB. Le défaut de 16 MiB est déjà valide. La valeur n'est pas relevée automatiquement |
| `unknown size suffix` ou `rate limit` | `rate_limit`, `RATE_LIMIT` ou `--rate-limit` n'a pas été reconnu. Le décimal est `20M`. Le binaire est `20Mi` ou `20MiB` |
| `head bucket "…"` | Le bucket manque, le point de terminaison est faux, ou la clé ne peut pas lire. Un échec de création sur la cible dit `create bucket "…" on target (need s3:CreateBucket)` |
| La source est encore chargée à `20M` | `20M` vaut 20000000 octets/s de corps lus sur la source. L'envoi est hors de ce plafond. Sur une carte partagée, baisser le nombre |
| `scan` ou `diff` n'a pas ralenti | Attendu. Le plafond s'applique pendant que `run` / `migrate` lisent les corps d'objet |
| Le scan reste lent après avoir augmenté `--workers` | Attendu. `scan` est monothread. La concurrence commence à la copie, et au HEAD de `diff` |
| Beaucoup de 503 ou de timeouts | Baisser `workers`. Pour les gros objets, baisser `multipart_concurrency` afin que `workers × multipart_concurrency` tienne dans le `maxconn` du proxy |
| Beaucoup de résultats `etag mismatch` | Vérifiez séparément le contenu de l’objet. Le découpage en parties et le chiffrement côté serveur peuvent influer sur les ETag. |
| `db_path is bound to bucket "…"` | Ce fichier appartient déjà à un autre bucket. Utiliser un autre `db_path` |
| `database is locked` | Deux processus écrivent la même base. Un processus par fichier |
| Le disque se remplit de fichiers temporaires | Déplacer `spool_dir` vers un grand disque, ou baisser `workers` |
| Ctrl-C ne revient pas | Attendre la requête en cours, ou appuyer encore (sortie 130) |
| `diff` signale encore `missing` | Pour une ligne `failed`, consultez `last_error` avant de réessayer. Pour une ligne `done` ou `conflict`, créez un nouvel inventaire dans un autre répertoire. En mode multibucket, changer le seul nom de fichier de la base ne suffit pas. |

## Codes de sortie

| Code | Sens |
| --- | --- |
| 0 | Opération terminée. Les conflits et les objets ignorés sont comptés séparément. |
| 1 | Configuration incorrecte, interruption, échec d’une opération ou d’une copie, ou clé manquante ou différente signalée par `diff`. |
| 130 | Un second `SIGINT` ou `SIGTERM`. Le processus quitte immédiatement |

Les erreurs de commande s'affichent sur stderr sous la forme `error: …`.

## Licence

Apache License 2.0. Copyright 2024-2026 RustFS, Inc.
