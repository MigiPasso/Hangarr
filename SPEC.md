# Hangarr : spécification

> Application Go + HTMX auto-hébergée qui croise **Tautulli** (historique de visionnage de tous les utilisateurs), **Plex** (bibliothèque, identifiants), **Sonarr / Radarr** (catalogue, taille, fichiers) et **Prowlarr** (santé des indexeurs), pour savoir ce qui est regardé, par qui, ce qui ne l'est jamais, et faire le ménage proprement.
>
> Le nom `hangarr` est provisoire et peut être renommé librement.

---

## 1. Objectifs

1. Voir pour chaque série / film : **dernier visionnage**, **nombre de lectures**, **jamais vu**, progression (épisodes vus / total).
2. **Tops** : séries et films les plus regardés (lectures, heures, par utilisateur).
3. Parcourir la bibliothèque en **grille d'affiches** avec des **filtres combinables** (statut de visionnage, ancienneté, genre, taille, tag…).
4. **Supprimer** via Sonarr / Radarr : série entière, **une saison**, ou un film, avec garde-fous (confirmation, dry-run, tag de protection, journal).
5. Aider à décider quoi supprimer : page **Nettoyage** avec un score et l'espace récupérable.
6. Accès protégé par **mire de login** : compte admin généré au premier démarrage, mot de passe à changer à la première connexion, politique de mot de passe robuste (section 11).
7. Livré comme **image Docker** destinée à tourner sous **Kubernetes** (section 13). **Développement et tests 100 % dans Docker**, rien d'installé sur la machine hôte (section 14).

---

## 2. Stack technique

| Élément | Choix | Raison |
|---|---|---|
| Langage | Go ≥ 1.23 | Routing stdlib `net/http` avec méthodes et patterns (`GET /series/{id}`) |
| Templates | `html/template` + `embed.FS` | Binaire unique, pas de build Node |
| Front | HTMX 2 (fichier vendorisé dans `static/`), CSS fait main avec variables, thème sombre par défaut | Pas de CDN, fonctionne hors ligne |
| Graphiques | SVG générés côté serveur | Pas de librairie JS |
| Base | SQLite via `modernc.org/sqlite` (pur Go, sans CGO) | Image Docker distroless simple |
| Migrations | Fichiers SQL embarqués, appliqués au démarrage | |
| Logs | `log/slog` (JSON en prod, texte en dev) | |
| Concurrence | `golang.org/x/sync/errgroup` | Sync parallèle des sources |
| Auth | `golang.org/x/crypto/argon2` (argon2id), sessions en base, `golang.org/x/text/unicode/norm` | Pas de lib d'auth tierce, surface minimale |
| Livraison | Dockerfile multi-stage → `gcr.io/distroless/static-debian12:nonroot`, multi-arch amd64/arm64, manifests Kubernetes (kustomize) | Section 13 |
| Dev / tests | `compose.dev.yml` : app en hot reload, faux serveurs Sonarr/Radarr/Plex/Tautulli, tests, lint, e2e Playwright | Section 14 |

Dépendances externes réduites au strict minimum. Pas de framework web.

---

## 3. Sources de données et API

Tous les appels se font **côté serveur** ; aucune clé d'API n'est jamais envoyée au navigateur.

> ⚠️ Les noms de champs ci-dessous sont ceux attendus. **Les valider sur de vraies réponses** (les enregistrer comme fixtures dans `testdata/`) et rendre les structs tolérantes (champs optionnels, pointeurs, valeurs par défaut).

### 3.1 Sonarr (API v3, header `X-Api-Key`)

| Usage | Endpoint |
|---|---|
| Catalogue séries | `GET /api/v3/series` : `id, title, sortTitle, year, tvdbId, imdbId, tmdbId, status (continuing/ended), network, genres, ratings, overview, images[] (coverType: poster/fanart, url, remoteUrl), seasons[] (seasonNumber, monitored, statistics{episodeFileCount, episodeCount, totalEpisodeCount, sizeOnDisk}), statistics{sizeOnDisk, episodeFileCount, totalEpisodeCount}, added, tags, qualityProfileId, path, monitored` |
| Tags | `GET /api/v3/tag` |
| Profils qualité | `GET /api/v3/qualityprofile` |
| Espace disque | `GET /api/v3/diskspace` |
| Affiches | `GET /api/v3/mediacover/{seriesId}/poster.jpg` (vérifier les variantes de taille dispo, ex. `poster-500.jpg`), sinon `remoteUrl` |
| **Supprimer série** | `DELETE /api/v3/series/{id}?deleteFiles=true&addImportListExclusion={bool}` |
| **Supprimer saison** | Pas d'endpoint dédié, séquence : |
| | 1. `GET /api/v3/episodefile?seriesId={id}` → filtrer `seasonNumber == N` |
| | 2. `DELETE /api/v3/episodefile/bulk` body `{"episodeFileIds": [...]}` |
| | 3. **Désactiver le monitoring de la saison** sinon Sonarr la retélécharge : `GET /api/v3/series/{id}`, passer `seasons[i].monitored=false`, `PUT /api/v3/series/{id}` avec l'objet complet |

### 3.2 Radarr (API v3, header `X-Api-Key`)

| Usage | Endpoint |
|---|---|
| Catalogue films | `GET /api/v3/movie` : `id, title, sortTitle, year, tmdbId, imdbId, hasFile, sizeOnDisk, genres, ratings{imdb,tmdb,…}, overview, images[], added, tags, qualityProfileId, studio, runtime, collection, movieFile{quality, mediaInfo}, path, monitored` |
| Tags / profils / disque | `GET /api/v3/tag`, `/qualityprofile`, `/diskspace` |
| Affiches | `GET /api/v3/mediacover/{movieId}/poster.jpg` (mêmes remarques) |
| **Supprimer film** | `DELETE /api/v3/movie/{id}?deleteFiles=true&addImportExclusion={bool}` |

### 3.3 Prowlarr (API v1, optionnel, page Santé)

`GET /api/v1/indexer`, `GET /api/v1/indexerstats` (requêtes, échecs, temps de réponse moyen par indexeur), `GET /api/v1/health`.

### 3.4 Plex (header `X-Plex-Token`, `Accept: application/json`)

| Usage | Endpoint |
|---|---|
| Bibliothèques | `GET /library/sections` → garder `type` = `movie` / `show` |
| Films | `GET /library/sections/{key}/all?type=1&includeGuids=1` : `ratingKey (string), title, year, Guid[] (imdb://…, tmdb://…, tvdb://…), viewCount, lastViewedAt, addedAt, duration, viewOffset` |
| Séries | `…/all?type=2&includeGuids=1` : `leafCount, viewedLeafCount, childCount, lastViewedAt, viewCount` |
| Saisons (1 seul appel) | `…/all?type=3` : `parentRatingKey, index, leafCount, viewedLeafCount, lastViewedAt` |
| **Historique** (tous comptes) | `GET /status/sessions/history/all?sort=viewedAt:desc` paginé via `X-Plex-Container-Start` / `X-Plex-Container-Size` : `ratingKey, parentKey, grandparentKey, type, title, grandparentTitle, index, parentIndex, viewedAt, accountID, librarySectionID` |
| Comptes | `GET /accounts` (id → nom) |
| Après suppression (optionnel) | `GET /library/sections/{key}/refresh` puis `PUT /library/sections/{key}/emptyTrash` |

> ⚠️ **Piège important** : `viewCount`, `lastViewedAt` et `viewedLeafCount` des endpoints `library` ne concernent que **le compte propriétaire du token**. **Tautulli est donc la source de vérité pour tout ce qui touche au visionnage** (section 3.5) : lectures, dernier visionnage, épisodes vus, progression par saison, par utilisateur. Plex sert au catalogue, aux GUID pour le rapprochement et à l'arborescence saisons/épisodes. L'historique Plex (`/status/sessions/history/all`) n'est utilisé qu'en repli si Tautulli n'est pas configuré (`HISTORY_SOURCE`).

### 3.5 Tautulli : source principale de l'historique

Tous les appels : `GET {TAUTULLI_URL}/api/v2?apikey={key}&cmd={commande}&…`, réponse enveloppée dans `response.result` (`success` / `error`) et `response.data`. Vérifier `result` systématiquement.

| Usage | Commande |
|---|---|
| **Historique** | `get_history` avec `grouping=0`, `order_column=date`, `order_dir=desc`, pagination `start` / `length` (lots de 1000). Lignes dans `data.data[]`, total dans `recordsFiltered`. Champs attendus : `id, reference_id, date, started, stopped, duration, play_duration, paused_counter, user_id, user, friendly_name, platform, player, media_type (movie/episode), rating_key, parent_rating_key, grandparent_rating_key, title, parent_title, grandparent_title, full_title, year, media_index, parent_media_index, percent_complete, watched_status, transcode_decision` |
| Utilisateurs | `get_users` : `user_id` (= id de compte Plex), `username`, `friendly_name`, `is_active`, `thumb` |
| Bibliothèques | `get_libraries` : `section_id, section_name, section_type` |
| **Lecture en direct** | `get_activity` : sessions en cours (utilisateur, titre, progression, appareil, transcodage, bande passante) |
| Contrôle de cohérence (optionnel) | `get_library_media_info?section_id=…` : `rating_key, play_count, last_played` agrégés tous utilisateurs, pour vérifier nos calculs |

Règles de calcul :
- On stocke les **sessions brutes** (`grouping=0`). Une **lecture** correspond à un `reference_id` distinct : Tautulli regroupe sous le même `reference_id` les sessions reprises d'un même média par un même utilisateur. `plays = COUNT(DISTINCT reference_id)`.
- Une **lecture complète** est un groupe dont au moins une session a `watched_status = 1` (seuil de Tautulli, 85 % par défaut, réglable dans Tautulli).
- **Temps regardé** : `play_duration` si présent, sinon `duration`, en secondes.
- Un **épisode vu** est un épisode avec au moins une lecture complète. La progression par saison et par série se calcule ainsi, globalement ou pour un utilisateur donné.
- Tautulli conserve l'historique des médias supprimés : on garde ces lignes (titre en clair) pour les stats, même sans média associé.

---

## 4. Rapprochement Plex ↔ Sonarr/Radarr

Ordre de priorité :
1. Séries : `tvdbId` → `tmdbId` → `imdbId`
2. Films : `tmdbId` → `imdbId`
3. Repli : titre normalisé (minuscules, sans accents ni ponctuation, sans article) + année ±1, marqué `match_method = "fuzzy"`

États stockés : `matched`, `arr_only` (dans Sonarr/Radarr, absent de Plex), `plex_only` (dans Plex, non géré par un *arr*).

Les éléments `plex_only` sont visibles (page Santé) mais **non supprimables** (pas de gestion de fichiers hors *arr*).

**Historique Tautulli → média** : les lignes d'historique portent les `rating_key` Plex.
- Film : `rating_key`.
- Épisode : `grandparent_rating_key` (série) + `parent_media_index` (saison) + `media_index` (épisode).

Les `rating_key` Plex pouvant changer (média retiré puis réajouté, réanalyse), le repli se fait sur le titre (`grandparent_title` ou `title`) + l'année avec la même normalisation. Les lignes restées orphelines sont comptées dans la page Santé.

---

## 5. Modèle de données (SQLite)

```sql
media (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,              -- 'movie' | 'series'
  arr_id INTEGER,                  -- id Sonarr/Radarr
  plex_rating_key TEXT,
  title TEXT, sort_title TEXT, year INTEGER, overview TEXT,
  tvdb_id INTEGER, tmdb_id INTEGER, imdb_id TEXT,
  status TEXT,                     -- continuing/ended (séries)
  genres TEXT,                     -- JSON array
  network_or_studio TEXT,
  rating REAL,
  size_bytes INTEGER,
  added_at INTEGER,                -- unix
  tags TEXT,                       -- JSON array de labels
  quality_profile TEXT,
  monitored INTEGER,
  path TEXT,
  has_file INTEGER,
  total_episodes INTEGER, downloaded_episodes INTEGER, watched_episodes INTEGER,
  runtime_minutes INTEGER,
  match_state TEXT, match_method TEXT,
  -- dénormalisés, recalculés à chaque sync :
  play_count INTEGER DEFAULT 0,          -- lectures (reference_id distincts), tous utilisateurs
  completed_play_count INTEGER DEFAULT 0,
  watch_seconds INTEGER DEFAULT 0,
  distinct_viewers INTEGER DEFAULT 0,
  last_viewed_at INTEGER,
  last_viewed_by INTEGER,                -- user_id Tautulli/Plex
  watch_state TEXT,                -- never | in_progress | completed | abandoned
  cleanup_score REAL,
  updated_at INTEGER,
  UNIQUE(kind, arr_id)
);

seasons (
  media_id INTEGER REFERENCES media(id) ON DELETE CASCADE,
  season_number INTEGER,
  plex_rating_key TEXT,
  monitored INTEGER,
  episode_file_count INTEGER, total_episode_count INTEGER,
  watched_episodes INTEGER, size_bytes INTEGER,
  last_viewed_at INTEGER,
  PRIMARY KEY (media_id, season_number)
);

watch_events (                     -- une ligne = une session brute Tautulli
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,            -- 'tautulli' | 'plex'
  source_id INTEGER NOT NULL,      -- id Tautulli (ou historyKey Plex)
  reference_id INTEGER,            -- regroupement en « lecture »
  media_id INTEGER REFERENCES media(id) ON DELETE SET NULL,
  media_type TEXT,                 -- movie | episode
  rating_key TEXT, parent_rating_key TEXT, grandparent_rating_key TEXT,
  season_number INTEGER, episode_number INTEGER,
  user_id INTEGER,
  started_at INTEGER, stopped_at INTEGER,
  play_seconds INTEGER,
  percent_complete INTEGER,
  watched INTEGER,                 -- watched_status = 1
  platform TEXT, player TEXT, transcode_decision TEXT,
  title TEXT, grandparent_title TEXT, year INTEGER,  -- conservés si le média disparaît
  UNIQUE(source, source_id)
);

plex_accounts (                    -- alimenté par Tautulli get_users
  account_id INTEGER PRIMARY KEY, username TEXT, friendly_name TEXT,
  is_active INTEGER, thumb_url TEXT
);

sync_cursors (source TEXT PRIMARY KEY, cursor TEXT, updated_at INTEGER);

-- Authentification de l'application (section 11)
app_users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,     -- chaîne PHC argon2id
  must_change_password INTEGER NOT NULL DEFAULT 1,
  password_changed_at INTEGER,
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  locked_until INTEGER,
  created_at INTEGER NOT NULL,
  last_login_at INTEGER
);

sessions (
  token_hash TEXT PRIMARY KEY,     -- SHA-256 du jeton, jamais le jeton lui-même
  user_id INTEGER NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  created_at INTEGER, last_seen_at INTEGER, expires_at INTEGER,
  ip TEXT, user_agent TEXT
);

auth_events (
  id INTEGER PRIMARY KEY, at INTEGER,
  user_id INTEGER, username TEXT,
  event TEXT,                      -- admin_created | login_success | login_failure | lockout
                                   -- | password_changed | password_reset_cli | logout | session_revoked
  ip TEXT, user_agent TEXT
);

action_log (
  id INTEGER PRIMARY KEY, at INTEGER, action TEXT,  -- delete_series | delete_season | delete_movie
  media_kind TEXT, arr_id INTEGER, title TEXT, season_number INTEGER,
  size_freed_bytes INTEGER, delete_files INTEGER, import_exclusion INTEGER,
  dry_run INTEGER, success INTEGER, error TEXT
);

sync_runs (id INTEGER PRIMARY KEY, started_at INTEGER, finished_at INTEGER,
           source TEXT, status TEXT, items INTEGER, error TEXT);
```

Index sur `media(kind, watch_state)`, `media(last_viewed_at)`, `media(size_bytes)`, `watch_events(stopped_at)`, `watch_events(media_id)`, `watch_events(user_id, media_id)`, `watch_events(reference_id)`.

---

## 6. Synchronisation

- Goroutine de fond, intervalle `SYNC_INTERVAL` (défaut 30 min), première sync au démarrage.
- Sources récupérées **en parallèle** (errgroup), timeout par requête, 2 retries avec backoff.
- Catalogue Sonarr/Radarr/Plex : rafraîchissement complet, upsert en transaction, suppression des éléments disparus.
- Historique Tautulli : **incrémental**. On pagine du plus récent au plus ancien et on s'arrête quand une page entière a un `date` antérieur au curseur moins 24 h. Le recouvrement rattrape les sessions longues terminées tardivement ; l'upsert sur `(source, source_id)` rend l'opération idempotente. Première sync : historique complet, avec progression affichée.
- Utilisateurs Tautulli rafraîchis à chaque sync.
- **Webhook Tautulli (optionnel, recommandé)** : `POST /webhooks/tautulli`, authentifié par le header `X-Hangarr-Token` (= `TAUTULLI_WEBHOOK_SECRET`). Il **ne fait pas confiance au contenu** : il déclenche seulement une sync incrémentale de l'historique, avec un anti-rebond de 30 s. À configurer dans Tautulli → Notification Agents → Webhook, déclencheurs « Playback Stop » et « Watched », URL du Service Kubernetes interne. Les stats sont ainsi à jour quelques secondes après la fin d'un épisode, sans attendre `SYNC_INTERVAL`.
- Après la sync : rapprochement, recalcul de `play_count`, `last_viewed_at`, `watch_state`, `cleanup_score`.
- Une erreur sur une source n'empêche pas les autres ; état visible dans la page Santé.
- Bouton « Synchroniser maintenant » (HTMX, `POST /sync`, statut rafraîchi par `hx-trigger="every 2s"` tant que la sync tourne).
- Commande CLI `hangarr sync --once` qui affiche un résumé (utile en dev).

### États de visionnage (`watch_state`)

| État | Film | Série |
|---|---|---|
| `never` | 0 lecture | 0 épisode vu |
| `in_progress` | au moins une session, aucune lecture complète | épisodes vus > 0 et < téléchargés, dernier visionnage < `ABANDON_DAYS` |
| `completed` | ≥ 1 lecture complète | tous les épisodes téléchargés vus |
| `abandoned` | commencé, jamais terminé, dernière session > `ABANDON_DAYS` | commencée, pas finie, dernier visionnage > `ABANDON_DAYS` (défaut 90) |

`watch_state` stocké = tous utilisateurs confondus. Quand un filtre utilisateur est actif, l'état est recalculé à la volée en SQL à partir de `watch_events` pour cet utilisateur.

---

## 7. Interface web

Principe HTMX : chaque page fonctionne en rendu complet ; si le header `HX-Request` est présent, le handler renvoie seulement le partial. Les filtres mettent à jour l'URL (`hx-push-url="true"`) pour que les vues soient partageables et que le bouton retour fonctionne.

Navigation : **Dashboard · Séries · Films · Nettoyage · Stats · Santé · Journal · Compte · Déconnexion**

Toutes les pages sont derrière la mire de login (section 11). Pages d'authentification :
- `GET/POST /login` : formulaire centré, sobre, message d'erreur générique.
- `GET/POST /account/password` : changement de mot de passe (forcé à la première connexion), checklist des règles en direct.
- `GET /account` : changer son mot de passe, liste des sessions actives (IP, navigateur, dernière activité) avec bouton « Révoquer ».
- `POST /logout`.

### 7.1 Dashboard
- **En cours de lecture** (Tautulli `get_activity`, rafraîchi toutes les 10 s via `hx-trigger="every 10s"`) : utilisateur, affiche, titre / épisode, barre de progression, appareil, direct play ou transcodage. Masqué s'il n'y a rien en cours.
- Cartes : nombre de séries / films, espace total, **espace occupé par du « jamais vu »**, espace récupérable (score > seuil), heures regardées sur 30 jours.
- Top 10 séries et top 10 films (lectures, 30 derniers jours / tout le temps, onglets HTMX).
- 10 derniers visionnages (affiche, titre, épisode, utilisateur, « il y a 2 h »).
- Activité 30 jours (barres SVG).
- Espace disque par dossier racine (`/diskspace`), barre de remplissage.
- Statut de la dernière sync.

### 7.2 Bibliothèque : Séries / Films
- **Grille d'affiches** (par défaut) ou **vue tableau** (toggle mémorisé en cookie).
- Carte : affiche (`loading="lazy"`), titre, année, badge d'état (Jamais vu / En cours 12/24 / Terminé / Abandonné), dernier visionnage relatif et par qui, taille, nombre de lectures, nombre de spectateurs distincts, cadenas si protégé, case à cocher de sélection.
- **Filtres** (formulaire `hx-get`, `hx-trigger="change, keyup changed delay:300ms from:input[name=q]"`) :
  - recherche texte
  - état de visionnage (multi)
  - dernier visionnage : jamais / > 30 / 90 / 180 / 365 jours
  - ajouté il y a : < 30 j / > 6 mois / > 1 an
  - genre (multi), année min / max, note min
  - tag *arr*, profil qualité, network / studio
  - statut série (en cours / terminée)
  - taille min
  - **utilisateurs** : vu par (multi), jamais vu par (multi), « vu par personne », nombre minimal de spectateurs distincts. Quand un utilisateur est sélectionné, badges, progression et dernier visionnage des cartes sont ceux de cet utilisateur.
  - état de rapprochement
- **Tri** : titre, dernier visionnage, date d'ajout, taille, lectures, note, score de nettoyage.
- Pagination par **scroll infini** (`hx-trigger="revealed"` sur la dernière carte), 48 éléments par lot.
- Barre de sélection collante : « 5 sélectionnés · 312 Go » → Supprimer la sélection.
- **Export CSV** de la liste filtrée courante.

### 7.3 Fiche détail (page dédiée, ouverte aussi en modal)
- Fanart en bandeau, affiche, synopsis, genres, note, qualité, chemin, tags, date d'ajout.
- Liens : ouvrir dans Sonarr / Radarr / Plex.
- **Séries, tableau des saisons** : monitorée, épisodes téléchargés / total, vus / total, dernier visionnage, taille, bouton « Supprimer la saison ».
- **Progression par utilisateur** : pour chaque spectateur, épisodes vus / total par saison, dernier épisode vu, prochain épisode.
- Historique de lecture : utilisateur, date, épisode, temps regardé, % de complétion, appareil, direct play / transcodage.
- Boutons de suppression.

### 7.4 Suppression (garde-fous)
- Modal de confirmation HTMX récapitulant : ce qui sera supprimé, espace libéré, options :
  - ☑ supprimer les fichiers (coché par défaut)
  - ☐ ajouter une exclusion de liste d'import (décoché par défaut)
- Série entière : saisie du titre requise pour confirmer (désactivable via `CONFIRM_BY_TYPING=false`).
- **Tag de protection** : tout média portant un tag listé dans `PROTECTED_TAGS` (défaut `keep`) côté Sonarr/Radarr est non supprimable (bouton désactivé + cadenas). Vérification **côté serveur** aussi.
- **`DRY_RUN=true` par défaut** : rien n'est supprimé, l'action est journalisée comme simulée ; un bandeau visible le rappelle.
- Suppression en lot : traitement séquentiel, rapport de résultat par élément.
- Après succès : la carte disparaît (`hx-swap="outerHTML"` vide), toast via header `HX-Trigger`, resync ciblée de l'élément, rafraîchissement Plex optionnel (`PLEX_REFRESH_AFTER_DELETE`).
- Toute action est écrite dans `action_log`.

### 7.5 Nettoyage
Liste triée par **score de nettoyage**, hors éléments protégés et hors délai de grâce (`GRACE_DAYS`, défaut 30 jours après ajout).

```
inactivité_j = jours depuis dernier visionnage (ou depuis ajout si jamais vu), plafonné à 730
score = (inactivité_j / 365) × log2(1 + taille_Go) / (1 + lectures)
        × 1.5 si jamais vu
        × 0.5 si série « continuing » avec épisodes vus dans les 60 derniers jours
```
Poids configurables. Affiche le total récupérable et permet la sélection / suppression en lot. Inclut aussi les **saisons jamais vues** de séries par ailleurs regardées.

### 7.6 Stats
- Top séries / films par **lectures** et par **heures regardées** ; filtre par période et par utilisateur.
- **Coût par lecture** : Go stockés / nombre de lectures (les plus « chers » en tête).
- **Genres stockés vs genres regardés** (« 40 % de l'espace en horreur, 5 % des lectures »).
- Répartition jour de la semaine × heure (heatmap SVG).
- **À reprendre** : séries en cours, avec le prochain épisode à voir.
- **Abandonnées** : séries commencées puis délaissées.
- **Page par utilisateur** : heures regardées par mois, top séries / films, genres préférés, séries en cours et abandonnées, appareils utilisés.
- Classement des utilisateurs (heures, lectures, nombre de titres différents).
- « Qui regarde quoi » : pour les séries les plus stockées, quels utilisateurs les regardent. Une série de 200 Go regardée par une seule personne qui a arrêté est un bon candidat au ménage.
- **Taux de complétion** : films commencés puis arrêtés avant le seuil de Tautulli.
- **Technique** : répartition direct play / direct stream / transcodage, plateformes et lecteurs les plus utilisés.

### 7.7 Santé
- Dans Plex mais pas dans Sonarr/Radarr ; dans Sonarr/Radarr sans fichier ou absent de Plex ; rapprochements approximatifs.
- Prowlarr : indexeurs, taux d'échec, temps de réponse moyen, alertes de santé.
- Historique des syncs et erreurs par source.

### 7.8 Journal
Deux onglets :
- **Actions** : table paginée de `action_log` (date, action, titre, saison, espace libéré, dry-run, succès / erreur).
- **Sécurité** : table paginée de `auth_events` (date, événement, utilisateur, IP, navigateur).

---

## 8. Images

- Route `GET /img/{kind}/{arrId}/{poster|fanart}` : proxy vers le MediaCover Sonarr/Radarr (repli `remoteUrl`).
- Cache disque dans `DATA_DIR/cache/img`, `Cache-Control: public, max-age=604800`, ETag.
- Placeholder SVG si aucune image.
- Limite de concurrence vers les *arr* (sémaphore) pour ne pas les saturer au premier affichage de la grille.

---

## 9. Arborescence du code

```
cmd/hangarr/main.go            # sous-commandes : serve, sync --once, admin reset-password, healthcheck, version
cmd/mockarr/                    # faux Sonarr/Radarr/Prowlarr/Plex/Tautulli pour le dev et les tests (section 14)
internal/auth/                  # hash, politique mdp, sessions, middleware, rate limit, bootstrap admin
internal/auth/common-passwords.txt  # liste embarquée de mots de passe courants
internal/config/                # env + validation
internal/clients/sonarr/
internal/clients/radarr/
internal/clients/prowlarr/
internal/clients/plex/
internal/clients/tautulli/
internal/store/                 # sqlite, migrations/*.sql, requêtes, filtres → SQL
internal/sync/                  # orchestration, historique incrémental
internal/matching/
internal/scoring/               # watch_state, cleanup_score
internal/actions/               # suppressions + garde-fous + journal
internal/web/                   # router, handlers, middleware (auth, CSRF, logs)
internal/web/templates/         # layout, pages/, partials/
internal/web/static/            # htmx.min.js, app.css, app.js (minime)
testdata/                       # fixtures JSON réelles anonymisées
e2e/                            # tests Playwright (exécutés dans leur conteneur)
deploy/k8s/                     # manifests kustomize
Dockerfile                      # stages : deps, dev, test, lint, build, runtime
compose.dev.yml                 # environnement de dev et de test
Makefile                        # raccourcis, chaque cible = une commande docker
```

Le module `store` construit les requêtes de filtre de façon **paramétrée** (jamais de concaténation de valeurs utilisateur).

---

## 10. Configuration (variables d'environnement)

| Variable | Défaut | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | |
| `DATA_DIR` | `/data` | SQLite + cache images |
| `SONARR_URL`, `SONARR_API_KEY` | requis | |
| `RADARR_URL`, `RADARR_API_KEY` | requis | |
| `PROWLARR_URL`, `PROWLARR_API_KEY` | optionnel | |
| `PLEX_URL`, `PLEX_TOKEN` | requis | |
| `TAUTULLI_URL`, `TAUTULLI_API_KEY` | requis si `HISTORY_SOURCE=tautulli` | source principale de l'historique |
| `HISTORY_SOURCE` | `auto` | `tautulli`, `plex` ou `auto` (Tautulli si configuré, sinon Plex) |
| `TAUTULLI_WEBHOOK_SECRET` | vide | active `POST /webhooks/tautulli` si défini (32 caractères min.) |
| `SYNC_INTERVAL` | `30m` | |
| `DRY_RUN` | `true` | |
| `PROTECTED_TAGS` | `keep` | liste séparée par virgules |
| `CONFIRM_BY_TYPING` | `true` | |
| `GRACE_DAYS` | `30` | |
| `ABANDON_DAYS` | `90` | |
| `PLEX_REFRESH_AFTER_DELETE` | `true` | |
| `ADMIN_USERNAME` | `admin` | nom du compte créé au premier démarrage |
| `COOKIE_SECURE` | `true` | `false` uniquement en dev local sans TLS |
| `SESSION_IDLE_TIMEOUT` | `12h` | expiration après inactivité |
| `SESSION_MAX_AGE` | `168h` | durée de vie absolue d'une session |
| `TRUSTED_PROXIES` | vide | CIDR des proxys/ingress autorisés à fournir `X-Forwarded-For` |
| `LOGIN_MAX_PER_IP` | `10` | tentatives par IP par fenêtre de 15 min |
| `LOG_FORMAT` | `json` | `json` ou `text` |
| `LOG_LEVEL` | `info` | |
| `TZ` | `Europe/Paris` | dates relatives et heatmap |

Toute variable secrète (`*_API_KEY`, `PLEX_TOKEN`, `TAUTULLI_WEBHOOK_SECRET`) accepte une variante `_FILE` (ex. `SONARR_API_KEY_FILE=/run/secrets/sonarr-api-key`) pour monter des Secrets Kubernetes en fichiers. Si les deux sont définies, démarrage en erreur.

---

## 11. Authentification

### 11.1 Premier démarrage : création du compte admin
- Au démarrage, après les migrations, si `app_users` est vide : création **dans une transaction** du compte `ADMIN_USERNAME`, avec `must_change_password = 1`. La contrainte `UNIQUE` garantit qu'il n'est jamais créé deux fois, même en cas de démarrage concurrent.
- Mot de passe initial : 24 caractères tirés avec `crypto/rand`, alphabet sans caractères ambigus (`0/O`, `1/l/I`), contenant obligatoirement minuscule, majuscule, chiffre et caractère spécial.
- Le mot de passe en clair est écrit **une seule fois**, sur stdout, en texte brut (même si `LOG_FORMAT=json`) pour rester lisible dans `docker logs` / `kubectl logs` :
  ```
  ================================================================
   HANGARR: INITIAL ADMIN ACCOUNT CREATED
   Username : admin
   Password : xxxxxxxxxxxxxxxxxxxxxxxx
   This password will never be shown again.
   It must be changed at the first sign-in.
  ================================================================
  ```
  Il n'est ni stocké, ni écrit ailleurs, ni relogué ensuite. Une ligne slog `admin_created` (sans le mot de passe) et une entrée `auth_events` sont ajoutées.
- Redémarrage, ou nouveau pod sur le même volume : **rien n'est régénéré ni affiché**.
- **Récupération** : `hangarr admin reset-password [--username admin]`, lancé via `docker exec` ou `kubectl exec`. Génère un nouveau mot de passe temporaire affiché **uniquement sur la sortie de la commande** (pas dans les logs du serveur), remet `must_change_password = 1`, déverrouille le compte, invalide toutes ses sessions, journalise `password_reset_cli`. SQLite en mode WAL + `busy_timeout` pour l'accès concurrent avec le serveur.

### 11.2 Politique de mot de passe (validée côté serveur, seule source de vérité)
- 12 à 128 caractères, Unicode accepté, normalisé en NFKC avant validation et hachage.
- Au moins 1 minuscule, 1 majuscule, 1 chiffre et 1 caractère spécial.
- Ne contient pas le nom d'utilisateur (insensible à la casse).
- Absent de la liste embarquée de mots de passe courants (~10 000 entrées, comparaison insensible à la casse).
- Différent du mot de passe actuel.
- Formulaire : checklist des règles mise à jour en direct par un petit script local (le mot de passe n'est pas envoyé au serveur à chaque frappe) ; les erreurs serveur s'affichent sous les champs via un swap HTMX.

### 11.3 Changement obligatoire à la première connexion
- Tant que `must_change_password = 1`, un middleware redirige toute route vers `/account/password`, sauf `/logout`, `/static/*`, `/healthz` et `/readyz`. Pour les requêtes HTMX : header `HX-Redirect`.
- Formulaire : mot de passe actuel, nouveau, confirmation.
- Succès : nouveau hash, `must_change_password = 0`, `password_changed_at`, **rotation de la session**, révocation de toutes les autres sessions, redirection vers le dashboard.

### 11.4 Hachage
- argon2id, paramètres par défaut `m=64 MiB, t=3, p=2`, sel de 16 octets, clé de 32 octets, stocké au format PHC (`$argon2id$v=19$m=65536,t=3,p=2$…`).
- Les paramètres étant dans la chaîne, re-hachage transparent à la connexion s'ils ont changé.
- Sémaphore limitant à 2 hachages simultanés, pour rester dans la limite mémoire du pod.

### 11.5 Sessions
- Jeton de 32 octets (`crypto/rand`, base64url) dans le cookie `hangarr_session` ; la base ne stocke que son SHA-256.
- Cookie `HttpOnly`, `Secure` (selon `COOKIE_SECURE`), `SameSite=Lax`, `Path=/`.
- Expiration après inactivité (`SESSION_IDLE_TIMEOUT`) et durée absolue (`SESSION_MAX_AGE`). `last_seen_at` mis à jour au plus une fois par minute.
- Nouvelle session à chaque connexion (pas de fixation de session) ; purge périodique des sessions expirées.

### 11.6 Protection contre le brute-force
- Message unique « Identifiants invalides », que l'utilisateur existe ou non.
- Utilisateur inconnu : vérification contre un hash factice pour un temps de réponse constant ; comparaisons en temps constant.
- Par IP : `LOGIN_MAX_PER_IP` tentatives par fenêtre de 15 min (en mémoire, valable car une seule réplique), puis HTTP 429.
- Par compte : après 5 échecs consécutifs, verrouillage progressif 1, 5, 15 puis 60 min (persisté dans `app_users`). `admin reset-password` déverrouille.
- IP réelle : `X-Forwarded-For` pris en compte uniquement si la connexion vient d'une adresse dans `TRUSTED_PROXIES`.
- Tous les événements vont dans `auth_events` (onglet « Sécurité » de la page Journal) et dans les logs.

## 12. Sécurité applicative

- Clés d'API uniquement côté serveur.
- Routes publiques limitées à : `/login`, `/static/*`, `/healthz`, `/readyz`. Le proxy d'images est lui aussi protégé.
- Exception : `POST /webhooks/tautulli`, sans session ni CSRF, mais :
  - désactivé (404) si `TAUTULLI_WEBHOOK_SECRET` est vide ;
  - secret comparé en temps constant ;
  - corps limité à 64 Ko et ignoré ;
  - rate limit ;
  - ne fait que déclencher une sync.
- **CSRF** sur tous les `POST` / `PUT` / `DELETE`, formulaire de login compris : jeton lié à la session (cookie de pré-session pour `/login`), envoyé par HTMX via `hx-headers` défini dans le layout.
- En-têtes de sécurité :
  - `Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`
  - HTMX compatible CSP via `<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false}'>` (styles d'indicateur dans `app.css`).
  - `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `Permissions-Policy` restrictive, `Strict-Transport-Security` si `COOKIE_SECURE=true`.
- `http.Server` avec `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` ; taille des corps de requête limitée.
- Vérification serveur du tag de protection et du dry-run, quelle que soit la requête reçue.
- Échappement automatique de `html/template` ; aucune donnée non échappée.

---

## 13. Image Docker et Kubernetes

### 13.1 Image
- `Dockerfile` multi-stage : `deps` (`go mod download` avec cache mounts BuildKit), `dev` (hot reload avec `air`), `test`, `lint`, `build` (`CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X main.version=…"`), `runtime` (`gcr.io/distroless/static-debian12:nonroot`).
- Utilisateur non-root UID/GID 65532, port 8080, seul répertoire inscriptible : `/data`.
- Compatible `readOnlyRootFilesystem` : base SQLite et cache d'images dans `/data`, `PRAGMA temp_store=MEMORY`.
- Pas de shell dans distroless : sous-commande `hangarr healthcheck` (appelle `/healthz` en local) pour le `HEALTHCHECK` Docker. Sous Kubernetes, sondes `httpGet`.
- Build multi-arch amd64/arm64 via `docker buildx`, labels OCI, version via `hangarr version`.

### 13.2 Comportement à l'exécution
- Configuration uniquement par variables d'environnement (et `_FILE` pour les secrets), logs JSON sur stdout.
- `GET /healthz` (liveness) : le processus répond.
- `GET /readyz` (readiness) : base ouverte et migrations appliquées. **Ne dépend pas** de la disponibilité de Sonarr/Radarr/Plex, pour ne pas faire tomber le pod quand l'un d'eux est indisponible.
- `SIGTERM` : arrêt propre (plus de nouvelles connexions, requêtes en cours terminées sous 20 s, sync annulée, base fermée).
- **Une seule réplique** à cause de SQLite : `replicas: 1`, `strategy: Recreate`, PVC `ReadWriteOnce`. Ne pas placer la base sur NFS/SMB.
- Ressources indicatives : requests `50m` / `128Mi`, limit mémoire `512Mi`.

### 13.3 Manifests (`deploy/k8s/`, kustomize)
`namespace`, `configmap`, `secret.example.yaml`, `pvc`, `deployment`, `service`, `ingress.example.yaml` (TLS), `networkpolicy.example.yaml` (egress limité à Sonarr/Radarr/Prowlarr/Plex/Tautulli + DNS ; ingress depuis l'ingress controller, plus depuis Tautulli si le webhook est utilisé).

Le Deployment impose : `runAsNonRoot`, `runAsUser/runAsGroup/fsGroup: 65532`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`, `automountServiceAccountToken: false`.

Récupérer le mot de passe initial : `kubectl -n hangarr logs deploy/hangarr | grep -A4 "INITIAL ADMIN"`. Les logs étant souvent collectés (Loki, etc.), le changement à la première connexion est obligatoire.

## 14. Développement et tests : tout dans Docker

**Seul prérequis sur la machine : Docker avec compose v2.** Aucun Go, Node, linter ou navigateur installé localement. Le `Makefile` n'est qu'un raccourci ; le README donne la commande `docker compose` équivalente pour chaque cible.

### 14.1 `compose.dev.yml`
| Service | Rôle |
|---|---|
| `app` | stage `dev`, source montée, hot reload, `DRY_RUN=true`, `COOKIE_SECURE=false`, pointe par défaut vers `mock` |
| `mock` | `cmd/mockarr` : faux Sonarr / Radarr / Prowlarr / Plex / Tautulli servant les fixtures `testdata/` (historique Tautulli paginé avec plusieurs utilisateurs, sessions reprises partageant un `reference_id`, médias supprimés, `get_activity` avec une lecture en cours). Enregistre les appels destructeurs (`DELETE`, `PUT`) et les expose sur `GET /_calls` (et `POST /_reset`) pour les vérifier |
| `test` | `go test -race -cover ./...` (CGO activé dans ce stage uniquement, pour `-race`) |
| `lint` | image officielle `golangci-lint` + `go vet` |
| `app-e2e` | instance de l'app sur base vierge (`tmpfs`), sans hot reload, pour les tests e2e |
| `e2e` | image officielle Playwright, scénarios dans `e2e/` |

Volumes nommés pour le cache des modules et du build Go. Profil `real` : utilise un `.env` (ignoré par git) pour pointer vers les vraies instances, toujours en `DRY_RUN=true` par défaut.

### 14.2 Cibles `make`
| Cible | Action |
|---|---|
| `make dev` | lance `app` + `mock`, UI sur `http://localhost:8080` |
| `make dev-real` | idem contre les vraies instances (profil `real`) |
| `make test` | tests unitaires et d'intégration |
| `make lint` | lint |
| `make e2e` | démarre `app-e2e` sur base vierge, **extrait le mot de passe initial depuis `docker compose logs app-e2e`**, le passe à `e2e` par variable d'env, lance Playwright, arrête tout |
| `make image` | build de l'image runtime multi-arch |
| `make k8s-validate` | `kustomize build` + `kubeconform` dans un conteneur |
| `make capture-fixtures` | conteneur qui interroge les vraies instances et écrit des fixtures anonymisées dans `testdata/` |
| `make reset-admin` | `docker compose exec app hangarr admin reset-password` |
| `make clean` | supprime conteneurs, volumes et base de dev |

### 14.3 Couverture de tests attendue
- **Unitaires** : rapprochement, `watch_state`, score, parsing des fixtures, construction des filtres SQL, politique de mot de passe (chaque règle), hash argon2id et re-hachage, verrouillage progressif, expiration des sessions.
- **Intégration** (`httptest`, SQLite temporaire) :
  - création de l'admin une seule fois, y compris avec deux démarrages concurrents ; mot de passe affiché une seule fois ;
  - middleware de changement forcé (pages normales et requêtes HTMX) ;
  - rejet des requêtes sans jeton CSRF ;
  - rate limit par IP et respect de `TRUSTED_PROXIES` ;
  - **séquence de suppression de saison** (fichiers supprimés puis saison démonitorée) et respect du dry-run et du tag de protection ;
  - historique Tautulli : sync complète puis incrémentale (arrêt au curseur, recouvrement idempotent), comptage des lectures par `reference_id`, lectures complètes, rapprochement par `rating_key` puis repli titre + année ;
  - webhook : refusé sans secret ou avec un mauvais secret, anti-rebond respecté.
- **e2e Playwright** : premier login avec le mot de passe des logs, refus d'un mot de passe faible, changement forcé, déconnexion / reconnexion, filtres de la bibliothèque, suppression d'une saison en dry-run puis en réel contre `mock` (vérifiée via `/_calls`), révocation de session.
- Les mêmes commandes doivent pouvoir tourner telles quelles en CI.

`README.md` : récupération du token Plex et des clés API, commandes de dev, déploiement Kubernetes, récupération du mot de passe admin.

---

## 15. Livraison par phases

1. **Socle Docker et authentification** : Dockerfile (tous les stages), `compose.dev.yml`, `Makefile`, `mockarr` minimal, config, store + migrations, auth complète (premier démarrage, login, changement forcé, sessions, CSRF, rate limit, en-têtes), `/healthz` et `/readyz`, arrêt propre, e2e des scénarios d'authentification.
2. **Données** : clients Sonarr / Radarr / Plex / Tautulli, sync (dont historique Tautulli incrémental et utilisateurs), rapprochement, scoring, `hangarr sync --once`, fixtures, `make capture-fixtures`.
3. **Bibliothèque** : layout, grille d'affiches, proxy d'images, filtres, tri, scroll infini.
4. **Détail et suppression** : fiche, saisons, modal de confirmation, dry-run, tag de protection, journal (actions + sécurité).
5. **Dashboard, Stats, Nettoyage**.
6. **Bonus et déploiement** : Santé + Prowlarr, webhook Tautulli, lecture en direct, stats techniques, export CSV, sélection en lot, manifests Kubernetes + `make k8s-validate`, image multi-arch, e2e complets.

Chaque phase se termine par : `make test`, `make lint` et `make e2e` verts, application lançable avec `make dev`.

---

## 16. Extrait de Deployment Kubernetes attendu

```yaml
spec:
  replicas: 1
  strategy:
    type: Recreate
  template:
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: hangarr
          image: registry.example.com/hangarr:1.0.0
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef:
                name: hangarr-config
          env:
            - name: SONARR_API_KEY_FILE
              value: /run/secrets/hangarr/sonarr-api-key
            # … idem RADARR, PROWLARR, PLEX, TAUTULLI, TAUTULLI_WEBHOOK_SECRET
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
          readinessProbe:
            httpGet: { path: /readyz, port: 8080 }
          resources:
            requests: { cpu: 50m, memory: 128Mi }
            limits: { memory: 512Mi }
          volumeMounts:
            - { name: data, mountPath: /data }
            - { name: secrets, mountPath: /run/secrets/hangarr, readOnly: true }
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: hangarr-data
        - name: secrets
          secret:
            secretName: hangarr-secrets
```
