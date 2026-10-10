# CLAUDE.md : conventions hangarr

`SPEC.md` est la spécification de référence (Go + HTMX, croise Plex / Tautulli / Sonarr / Radarr).
**Prowlarr est hors périmètre** (décision utilisateur) : aucun client, aucune config, aucune page ; ignorer SPEC §3.3,
la partie Prowlarr de §7.7 et `PROWLARR_*` de §10.
**Pas de « lectures en cours »** (décision utilisateur) : ignorer `get_activity` (SPEC §3.5) et le bloc « En cours de
lecture » du Dashboard (§7.1).
L'application s'appelle **Hangarr** (affichage) ; dépôt, binaire, module Go, image et préfixe des variables : `hangarr` / `HANGARR_`
(décision utilisateur : « tour de contrôle » de la stack *arr, plus seulement du nettoyage). Anciens noms : « arrstats »
(rien de gardé) puis **Cleanarr**, qui était déployé : compatibilité gardée et testée (`CLEANARR_MASTER_KEY(_FILE)` lue
si `HANGARR_*` absente, avec avertissement ; `cleanarr.db` renommée au démarrage, `store.renameLegacyDB` ; en-tête
`X-Cleanarr-Token` accepté) ; procédure de migration : `docs/deploiement.md` §11. Ne pas retirer ces replis sans
accord. Migration 0006 : mentionne encore Cleanarr (jamais modifier une migration livrée).

## Règle n°1 : tout passe par Docker

Rien ne s'exécute sur la machine hôte en dehors de `docker`, `docker compose`, `make` et `git`.
**Jamais** de `go`, `gofmt`, `npm`, `npx`, `node`, `golangci-lint`, `playwright` ni de navigateur en local.

| Besoin | Commande |
|---|---|
| Lancer l'app (hot reload + mocks) | `make dev` → http://localhost:8080 (`DEV_PORT=8081 make dev` si le port est pris ; `MEDIA=jellyfin` (Jellyfin du mock) ou `MEDIA=none` (serveur configuré dans l'UI)) |
| Contre les vraies instances | `make dev-real` (lit `.env`, `DRY_RUN=true` par défaut) |
| Tests unitaires et d'intégration (`-race`) | `make test` |
| `go vet` + golangci-lint | `make lint` |
| e2e Playwright (image runtime, base vierge) | `make e2e` |
| Vérification compacte (gofmt + lint + tests, `E2E=1` pour e2e), logs complets dans `.verify/` | `make verify` |
| `go mod tidy` | `make deps` |
| Autre commande Go ponctuelle | `docker compose -f compose.dev.yml --profile tools run --rm gotools <cmd>` (ex. `gofmt -l -w .`) |
| npm pour e2e | `docker compose -f compose.dev.yml --profile e2e run --rm --no-deps e2e npm …` |
| Télécharger htmx / la liste de mots de passe | `make vendor-assets` |
| Régénérer les PNG du favicon depuis `static/favicon.svg` | `make icons` |
| Image runtime locale / multi-arch | `make image-load` / `make image` (`PUSH=1` pour pousser) |
| Vérifier le déploiement Docker (`compose.yml`) | `make deploy-check` |
| Lancer une sync dans l'app de dev | `make sync` (`hangarr sync --once`) |
| Capturer de vraies réponses API (anonymisées) | `make capture-fixtures` (lit `.env`, écrit `testdata/captured/`) |
| Réinitialiser le mot de passe admin (dev) | `make reset-admin` |
| Tout supprimer (conteneurs, volumes, base de dev) | `make clean` |

Versions épinglées : Go dans `Dockerfile` (`GO_VERSION`) **et** `compose.dev.yml` (service `gotools`) ; golangci-lint et air
dans `Dockerfile` ; Playwright dans `compose.dev.yml` **et** `e2e/package.json` (mêmes versions) ; htmx dans `Makefile`.
Le Makefile doit rester compatible GNU Make 3.81 (macOS).

## Livraison

- Par phases (SPEC §15). Fin de phase : `make test`, `make lint` et `make e2e` verts, `make dev` fonctionnel, court récap,
  puis **attendre la validation de l'utilisateur** avant la phase suivante.
- Si un point de la spec est ambigu ou semble une mauvaise idée : le dire et proposer une alternative, ne pas deviner.

## Stack (SPEC §2) : ne pas en sortir

stdlib `net/http` (routing par patterns `GET /x/{id}`), `html/template` + `embed`, HTMX 2 vendorisé dans
`internal/web/static/`, CSS fait main, SVG générés côté serveur, `modernc.org/sqlite` (sans CGO), migrations SQL
embarquées (`internal/store/migrations/NNNN_name.sql`, jamais modifier une migration déjà livrée), `log/slog`.
Pas de framework web, pas de build Node pour l'app, pas de CDN.

## Conventions de code

- Code, identifiants, commentaires, logs et CLI en **anglais**. Interface **bilingue, anglais par défaut** (décision
  utilisateur) : `internal/i18n`, clés = texte anglais (`{{t "Dashboard"}}`, `{{tn n "%d item" "%d items"}}`,
  `l.T(...)` / `l.N(...)` en Go, suffixe `|contexte` pour distinguer deux textes anglais identiques), catalogue
  `internal/i18n/fr.go`, `TestFrenchCatalogComplete` vérifie chaque clé. Langue : cookie `hangarr_lang` (lien EN / FR,
  `GET /lang/{code}?next=` limité aux chemins locaux), sinon `DEFAULT_LANG` (`en`). Tests web et instances e2e en
  `DEFAULT_LANG=fr`. Nombres, tailles, dates et durées relatives via le `Localizer`. Titres dynamiques : `pageRaw`.
- Aide contextuelle : explications courtes derrière un « i » (`{{template "info" (t "…")}}`, bulle CSS au survol et au
  focus), **toujours à côté** du titre ou du `<label>` (`.title-row`), jamais dedans (sinon le texte d'aide entre dans
  le nom accessible). Explications concises. Bulle cachée = `display: none` (sinon elle élargit les panneaux défilants) ;
  dans `.title-row` elle s'ouvre sous la ligne, alignée à gauche. Bouton + « i » : envelopper dans `.with-info`. Pas de
  « i » sur Protéger (explication en `title` du bouton, décision utilisateur).
- Tableaux : toujours dans `<div class="table-wrap">` (défilement horizontal contenu, `position: relative` pour les
  en-têtes `visually-hidden`) ; aucune page ne doit défiler sur le côté en mobile (test e2e « no page scrolls sideways »).
- Champs numériques : stepper `.stepper` (boutons `data-step-by` + `aria-controls`, `app.js`), pas les flèches natives.
- SQL toujours paramétré ; les requêtes vivent dans `internal/store`.
- Pages : rendu complet, ou partial si `HX-Request` est présent. Redirections HTMX via `HX-Redirect` (`redirect()`).
- Templates : une page = `templates/pages/<nom>.html` (déclarée dans `pages` de `views.go`) + `layout.html` + `partials/*`.
- CSP stricte (`script-src 'self'; style-src 'self'`) : **aucun** `<script>` inline, `style="…"` ni handler `on…=`.
  Pour les barres de progression : `<progress>`, `<meter>` ou SVG.
- CSRF : tout POST/PUT/DELETE authentifié passe par `authed()` ; le layout envoie le jeton via `hx-headers`,
  les formulaires classiques via le champ caché `csrf_token`.
- Clients d'API (phase 2+) : structs tolérantes (champs optionnels, pointeurs), fixtures réelles anonymisées dans
  `testdata/`, `make capture-fixtures` pour les régénérer. Les noms de champs de SPEC §3 sont à valider.

## Invariants de sécurité et de données

- **Serveur multimédia : Plex OU Jellyfin** (décision utilisateur, 2026-10-10). Un seul configuré à la fois : la carte de
  l'autre (et Tautulli avec Jellyfin) est grisée, `Manager.Save`/`Test` refusent (`upstream.Disabled`), vider reste permis ;
  `PLEX_*` + `JELLYFIN_*` dans l'env = refus de démarrer ; les deux configurés quand même (env + interface) =
  `Clients.ServerConflict` : pas de sync (`Ready()` faux), suppressions via le serveur refusées, bandeau ; mode migration
  envisagé plus tard. Pastille du serveur utilisé dans la barre du haut (`server-chip`,
  dans `sync_status`, donc rafraîchie hors bande ; absente sans serveur). L'interface nomme le serveur actif (fonction de
  template `server`, clés i18n à `%s`). **Rien n'est purgé au changement** : colonnes distinctes (`plex_rating_key`,
  `jellyfin_id`, migration 0007), la sync ne réécrit que celles du serveur lu (`store.ApplyServerMatches`), les éléments
  connus du seul autre serveur passent en `match_state = 'hidden'` (gelés, masqués de la Bibliothèque / Santé / lot,
  protection locale et historique conservés, aucune action possible : `actions.ErrInactiveServer`). `plex_only` est
  devenu `server_only` (alias accepté dans `?match=`). Historique Jellyfin natif (`jellyfin_history`) : dernière
  lecture de chaque élément par utilisateur (`Played` + lectures en cours `IsResumable`), pas de durées, curseur par
  utilisateur ; utilisateurs rattachés **par nom** aux spectateurs existants (`store.AccountIDsByName`), sinon id
  `jellyfin.AccountID` (≥ 1<<62). Le ménage de l'historique Plex (`DropOtherHistory`, `KeepViewers`) ne touche que
  `tautulli`/`plex_history`. Dédoublonnage Plex ↔ Jellyfin (décision utilisateur) : `store.MergeServerHistories` à
  chaque sync (après `MergePlexHistory`, quel que soit le serveur actif) supprime l'événement Jellyfin qui répète une
  lecture Plex/Tautulli (même spectateur, élément, épisode ; de 30 min avant à 24 h après, ou import en masse :
  ≥ 20 éléments vus dans la même minute, sans condition de date) et complète la lecture Plex partielle si aucune lecture
  Plex terminée ne l'explique. Tautulli et `HISTORY_SOURCE` : Plex seulement. Client Jellyfin : en-tête
  `Authorization: MediaBrowser Token="…"` (pas `X-Emby-Token`/`api_key`, désactivés par Jellyfin 12) ; Emby refusé.
  Labo réel : `make jellyfin-lab` (`deploy/jellyfin-lab`, image épinglée, projet `hangarr-jellyfin-lab`, port local).
- `DRY_RUN=true` par défaut ; dry-run et tag de protection (`PROTECTED_TAGS`) vérifiés **côté serveur**.
- Suppression de saison : supprimer les fichiers **puis** démonitorer la saison (`PUT` de la série complète).
- Visionnage = **historique fusionné** (`HISTORY_SOURCE=auto` → `merged` si Tautulli est configuré, décision
  utilisateur) : l'historique Plex (`/status/sessions/history/all`) dit ce qui a été vu ; Tautulli enrichit les lectures
  qu'il connaît (durées, appareils, lectures partielles). Doublons rapprochés par utilisateur + élément + date
  (`store.MergePlexHistory`, après `linkEvents`) ; lecture connue de Plex seul : durée estimée depuis la durée Sonarr/Radarr.
  Spectateurs = **utilisateurs Tautulli** (décision utilisateur : `/accounts` de Plex contient des comptes fantômes) ;
  comptes Plex rattachés à l'id Tautulli par nom (le propriétaire vaut 1 côté Plex), comptes Plex inconnus de Tautulli
  écartés avec leurs lectures (`store.KeepViewers`). Modes `tautulli` / `plex` : une
  seule source, l'autre est purgée. Ne jamais utiliser `viewCount` / `lastViewedAt` / `viewedLeafCount` de Plex : ils
  ne concernent que le compte du token.
- Auth (SPEC §11) : admin créé une seule fois au premier démarrage, mot de passe affiché une seule fois sur stdout,
  changement forcé ; sauf `HANGARR_ADMIN_PASSWORD` (décision utilisateur, variable seule **sans `_FILE`**) : mot de
  passe choisi, soumis à la politique (sinon refus de démarrer, même compte existant), ni affiché ni à changer, utilisé seulement si le compte
  n'existe pas (sinon ignoré avec avertissement) ; dev : `admin` / `Hangarr-Dev-1` (`compose.dev.yml`), e2e :
  `app-e2e-live` (pas de bannière, vérifié par `make e2e`), argon2id (PHC), sessions stockées hachées (SHA-256), verrouillage progressif, rate limit par IP,
  `X-Forwarded-For` seulement depuis `TRUSTED_PROXIES`.
- Requêtes sortantes : clients amont via `httpx` uniquement (redirections suivies seulement vers le même hôte,
  `httpx.SameHostOnly`, sinon la clé partirait ailleurs) ; images distantes : https seulement, redirections https, 3 max.
  Jamais `http.DefaultClient` pour un appel authentifié.
- URL publiques : refusées si c'est une page de l'appli (`/login`, `/movie/…`, requête ou ancre ; Plex `/web`),
  `upstream.pageOfTheApp` ; un préfixe (`https://hôte/radarr`) reste accepté.
- Identifiants conservés (champ vide) : jamais envoyés à une autre origine. Si l'URL d'API d'un service change de
  schéma, d'hôte ou de port, ses identifiants doivent être ressaisis (enregistrement **et** test de connexion,
  `Manager.keptCredentials`) ; identifiant défini par l'environnement → adresse aussi par l'environnement. Sinon une
  session suffirait à rediriger le token Plex ou une clé d'API vers un hôte tiers (audit 1.0.0).
- Rate limit par IP partagé entre connexion et changement de mot de passe ; CSP avec `object-src 'none'; base-uri 'none'`.
- Kubernetes : image distroless nonroot (65532), rootfs en lecture seule, seul `/data` est inscriptible,
  `/healthz` (liveness) et `/readyz` (readiness, sans dépendre des *arr*), arrêt propre sur SIGTERM (20 s).
  Une seule réplique (SQLite).

## Repères

- `cmd/hangarr` : `serve`, `sync --once`, `admin reset-password`, `master-key`, `backup [PATH]`, `healthcheck`, `version`.
- `internal/mockarr` (servi par `cmd/mockarr`) : faux upstreams (sonarr :8989, radarr :7878, plex :32400,
  jellyfin :8096, tautulli :8181) depuis `testdata/`, appels destructeurs exposés sur `GET /_calls`, `POST /_reset`.
  Les tests d'intégration l'utilisent via `httptest`. `MOCK_SHIFT_DATES=true` recale l'historique sur aujourd'hui.
  Dev seulement (`compose.dev.yml`) : `MOCK_DEV_EXTRAS` (catalogue `testdata/dev/`) et `MOCK_REAL_POSTERS` (vraies
  affiches de `testdata/posters.json` et vraies images de fond TMDB de `testdata/fanarts.json`, téléchargées par le
  mock ; seulement les URL sont versionnées) ; jamais dans les tests (pas d'Internet, comptes stables).
- `testdata/` : scénario synthétique décrit dans `testdata/README.md` (les tests en dépendent) ;
  `testdata/captured/` : vraies réponses anonymisées, ignorées par git (jamais commitées), seulement vérifiées au décodage.
- `internal/syncer` (et non `internal/sync`, pour ne pas masquer le paquet standard) : fetch parallèle, stockage,
  rapprochement (`internal/matching`), liaison de l'historique, recalcul (`internal/scoring`).
- Rapprochement Plex ↔ *arr (`internal/matching`) : IDs (TVDB/TMDB/IMDb) → **même fichier ou dossier** (nom de base,
  insensible aux montages ; `Media/Part/file` et `Location` côté Plex, `path` + `movieFile.relativePath` côté *arr, non
  stockés : indices passés par le syncer ; nom partagé par plusieurs éléments ignoré) → titre (+ titre original /
  alternatifs) et année ±1. Cas réel : films français titrés en anglais dans Radarr, sans IDs côté Plex.
- Saison 0 (spéciaux) exclue de la progression et de `watch_state`. Série dont la **dernière saison avec fichiers est
  finie** = `completed` (saisons précédentes possiblement vues ailleurs, décision utilisateur) : `scoring.WatchState` et
  SQL de `libraryView`.
- Filtres de la Bibliothèque **simplifiés** (décision utilisateur, « un filtre = une question ») : Qui ? (`user`, tout
  devient le point de vue de ce spectateur), État, Présent dans (Plex et/ou Sonarr/Radarr, juste sous État, décision
  utilisateur), Pas regardé depuis (`inactive` 30/90/180/365, jamais vu = depuis l'ajout, même règle que Nettoyage),
  Ajouté, Taille min/max, Statut ; le reste dans « Plus de filtres » (genres, année, note, tags, profil, réseau/studio). Supprimés : Vu par / Jamais vu par / Vu par personne /
  spectateurs distincts min. Nettoyage : « Pas regardé depuis » ne propose que des délais plus longs que la règle.
- Bibliothèque : une requête SQL paramétrée par lot (`store.LibraryQuery`, CTE `v` avec colonnes `eff_*` ; point de vue
  utilisateur calculé à la volée, mêmes règles que `scoring`). Recherche sur `title_norm` (sans accents ni article).
  Recherche déclenchée par `input` (et non `keyup` comme dans la spec) pour couvrir collage, autocomplétion, bouton ✕.
- Sync : planning réglé dans l'UI (page Réglages, table `app_settings`) : heure (`sync_time`, défaut `SYNC_TIME`=04:00,
  fuseau `TZ`) + fréquence (`sync_every_hours` ∈ 24/12/8/6/4/3/2/1, diviseurs de 24 → mêmes horaires chaque jour,
  défaut 24 = une fois par jour) ; `syncer.Schedule`, boucle `Syncer.Loop`. Rattrapage au démarrage si la dernière sync
  est plus vieille que l'intervalle. `SYNC_INTERVAL` de la spec est supprimé.
- Suppressions : `internal/actions` (tag de protection relu en live sur Sonarr/Radarr, dry-run, titre à saisir pour une
  série, journal `action_log` pour toute tentative). Saison : fichiers (`DELETE episodefile/bulk`) PUIS `PUT` de l'objet
  série **brut** avec `monitored=false` (`sonarr.UnmonitorSeason`). Resync ciblée ensuite (`syncer.RefreshSeries`).
- Suppression via le serveur multimédia actif (`actions.deleteFromServer`, actions `delete_plex` / `delete_jellyfin`) :
  élément connu de ce serveur seul (plus dans Sonarr/Radarr, sa clé présente, pas `hidden`) → Plex
  `DELETE /library/metadata/{ratingKey}` (refus 401/403 = « Autoriser la suppression des médias » désactivé côté Plex,
  expliqué) ou Jellyfin `DELETE /Items/{id}` ; le serveur supprime les fichiers ; puis `PLEX_REFRESH_AFTER_DELETE`
  rescanne le serveur actif (Jellyfin : `POST /Library/Refresh`, une fois par lot). Fiche seulement (pas en lot, pas de
  saison), dry-run et titre à saisir pour une série respectés. Protection de ces éléments **dans Hangarr seulement**
  (décision utilisateur) : colonne `media.protected_local` (migration 0006, jamais écrite par la sync), rien envoyé à
  Plex, appliquée même en dry-run, journalisée ; `protectedClause` et les templates (`.ProtectedLocal`) en tiennent compte.
- Fiche détail : progression exacte via `GET /api/v3/episode?seriesId=` (appel live, 5 s max, repli approximatif).
  Liens : `SONARR_PUBLIC_URL`, `RADARR_PUBLIC_URL`, `PLEX_PUBLIC_URL` (optionnels ; Plex par défaut via app.plex.tv).
- Phase 5 : Dashboard (sans lectures en cours), Stats (/stats, /stats/users/{id}), Nettoyage (/cleanup). Graphiques SVG
  générés en Go (`internal/web/charts.go`), palette validée (skill dataviz) : bleu #3987e5, orange #d95926, rampe bleue
  pour la heatmap ; chaque graphique a une info-bulle `<title>` et un tableau de données. **Pas de score de nettoyage**
  (décision utilisateur, remplace SPEC §7.5) : candidat = géré par Sonarr/Radarr, avec fichiers, non protégé, et personne
  ne l'a regardé depuis N jours (jamais vu : depuis `added_at`), N réglable dans Réglages (`cleanup_inactive_days`,
  défaut 180). Règle évaluée en SQL (`store.CleanupScope`), liste triée par taille avec la raison en clair.
  Filtres latéraux sur Nettoyage (type, états, lectures min/max, taille min/max en Go, `store.CleanupFilter`), conservés
  par les liens de tri ; saisons jamais vues masquées si un filtre d'état ou de lectures est actif.
  `GRACE_DAYS` supprimé (couvert par la règle) ; colonne `cleanup_score` laissée à NULL (migration déjà livrée).
  Heure de sync : deux `<select>` heure (00-23) / minute (00-59, toutes les valeurs), saisie au clavier native ; pas de
  `datalist` (deux clics pour ouvrir, rejeté par l'utilisateur).
- e2e : `app-e2e` (DRY_RUN=true) et `app-e2e-live` (DRY_RUN=false) partagent le mock ; `/_calls` vérifie les appels.
- `internal/upstream` : configuration effective des services (env prioritaire et verrouillée, sinon `app_settings`
  clés `svc.*`, secrets scellés par `internal/secretbox` avec le nom du champ en AAD). `Registry` = instantané atomique des
  clients : syncer, actions, images et web appellent `Get()` à chaque opération, jamais de client gardé en champ.
  `Manager.Save` écrit + journalise (`auth_events`, `config_<service>`) dans une transaction puis `Reload`.
  Dev : clé maître fixe dans `compose.dev.yml` ; e2e : instance `app-e2e-config` sans variables de service, clé via
  secret Docker (`e2e/master.key`, test uniquement).
- Protection (`actions.SetProtected`, `POST /{series|movies}/{id}/protect`, champ `protect=1|0`) : tags relus en live,
  tag `PROTECTED_TAGS[0]` créé s'il manque (`POST /api/v3/tag`), retrait = tous les tags de protection, autres tags
  conservés, `PUT` de l'objet brut. Sans changement (déjà dans l'état demandé) : rien d'envoyé ni journalisé, mais copie
  locale des tags réalignée. Le mock enregistre aussi les `POST` dans `/_calls`.
- À venir (`/calendar`, `internal/web/calendar.go`) : `GET /api/v3/calendar` de Sonarr (`includeSeries`) et Radarr,
  `unmonitored=false`, lu en direct (8 s max par service, en parallèle), rien en base ; page envoyée tout de suite,
  sorties chargées par HTMX (`hx-trigger="load"`, `hx-select="#calendar-page"`), `live=1` sans JS (tests : en-tête
  `HX-Request`) ; URL construites par `url.Values` (paramètres triés) ; trois vues (bascule Liste / Grille / Calendrier) : liste ou grille d'affiches (`view=grid`, cartes
  de la Bibliothèque en **un seul flux** : une tuile date (jour, numéro, mois, « dans N jours ») dans le flux avant les
  cartes de chaque jour, pour des rangées pleines), paginées (période 30 /
  90 (défaut) / 365 jours, `size` 10 / 25 (défaut) / 50 / 100 dans un petit `<select>` de la barre de pagination en bas, soumis par HTMX au
  `change` ; `page`, liens qui gardent période et taille), ou vue mois (`view=month&month=2026-10`, semaines lundi en premier, mois précédent / suivant, pas de lien « ce mois-ci » (décision utilisateur) ; sous
  760 px la grille devient la liste des jours qui ont des sorties). Épisodes d'une même saison le même jour regroupés (`S02E01–E03`), premières et finales (`finaleType`)
  signalées ; une ligne par date de sortie Radarr (cinéma, numérique, physique), dates gardées telles quelles (jours,
  pas d'heure). Lien vers la fiche si l'élément est déjà synchronisé. Le mock recale toujours `calendar.json` sur
  aujourd'hui.
- Santé (`/health`) : services vérifiés en direct (`upstream.CheckAll`, chargés en différé par HTMX, alertes
  `/api/v3/health` de Sonarr/Radarr hors indexeurs, partagées entre les deux sections : singleflight + 15 s par
  instantané de clients), section **Trackers** séparée (`upstream.CheckTrackers`,
  `/health/trackers` en différé : `/api/v3/indexer` ; **Sonarr/Radarr n'ont pas d'API d'état des indexeurs**
  (`/indexerstatus` = Prowlarr) : « en échec » = nom cité par l'alerte `IndexerStatusCheck` /
  `IndexerLongTermStatusCheck` (« …: A, B », nom exact entre séparateurs, indépendant de la langue ; sans nom = tous) ;
  en échec d'abord, alertes `Indexer*` ; champs `fields` des indexeurs jamais décodés ni capturés, ils contiennent des
  clés), derniers passages et historique de `sync_runs`, écarts de données
  (`store.DataHealth`).
- Webhook Tautulli (`POST /webhooks/tautulli`, seule route sans session) : 404 sans secret, `X-Hangarr-Token` comparé
  en temps constant, corps ignoré, 60 req/min/IP, puis `syncer.RequestHistorySync` (anti-rebond 30 s, historique seul,
  partage le verrou de la sync complète). `LastSyncAt` ne compte que les sources de catalogue. Secret généré dans
  Configuration (affiché une seule fois).
- `internal/images` : proxy MediaCover (`poster-500.jpg`) → cache disque `DATA_DIR/cache/img`, ETag, sémaphore (4),
  toute image réduite avant mise en cache (`resize.go`, bibliothèque standard, filtre boîte, JPEG q85) : affiches
  500 px, fonds **1280 px** de large, quelle que soit la source (décision utilisateur) ; plus de 25 Mpx refusé (bombe de
  décompression), un seul décodage à la fois (limite mémoire du pod), format non décodable (WebP) servi tel quel ; noms de fichiers versionnés (`cacheVersion`), anciennes
  versions supprimées au démarrage. Volume : 5 Gi par défaut dans le manifeste (10 Gi pour une grosse bibliothèque),
  singleflight, repli `remoteUrl` https, placeholder SVG. Pas de JS supplémentaire côté client. Éléments Plex seul :
  `/img/plex/{ratingKey}/{poster|fanart}` (clé vérifiée en base), `/photo/:/transcode` (500×750 / 1280×720) puis
  `/library/metadata/{key}/thumb|art` ; template `imgURL` choisit *arr ou Plex.
- Sélection en lot (bibliothèque grille/tableau + Nettoyage) : cases `name=ids form=batch-form` (absentes si protégé ou
  Plex seul), barre collante `partials/batch.html`, `POST /batch/delete` (1er POST = confirmation, `step=run` = exécution ;
  série dans le lot + `CONFIRM_BY_TYPING` → saisir le nombre d'éléments), `POST /batch/protect` (sans confirmation).
  `actions.DeleteBatch/ProtectBatch` : séquentiel, `Delete` normal par élément (`Confirmed`, `SkipPlexRefresh`), un seul
  rafraîchissement Plex par type à la fin, 100 éléments max, délai d'écriture prolongé (10 min). Rapport par élément dans la
  modale `#confirm`, événement `hangarr:batch-done` (cartes supprimées retirées, rechargement à la fermeture après protection).
- Déploiement Docker : `compose.yml` (prod, projet `hangarr`, volume `hangarr-data`, durci comme k8s, `.env` facultatif),
  `make deploy-check` = `compose.yml` tel quel + `deploy/compose.check.yml` (mock, sans port ni `.env`) + `deploy/check.sh`.
  `hangarr backup [PATH]` (`VACUUM INTO`, à chaud). Avertissement sur la page de connexion si HTTP avec `COOKIE_SECURE=true`
  (hors localhost). Compose ≥ 2.24 (`env_file.required`, `!reset`).
- Navigation : **menu latéral à gauche façon Sonarr** (`partials/nav.html`, templates `sidebar`, `topbar`) : Tableau de
  bord, Bibliothèque (Séries, Films), À venir, Nettoyage, Stats, Système (Santé, Journal), Réglages (Général, Services,
  Compte) ; une section n'affiche ses pages que si l'on y est, son titre mène à la première. Icônes SVG en ligne
  (template `icon`). Réductible aux icônes (bouton, cookie `hangarr_sidebar=collapsed` lu par le serveur, pas de flash ;
  libellés gardés pour les lecteurs d'écran) ; sous 800 px, panneau ☰ par-dessus la page (Échap, fond cliquable), sans
  JS le menu s'affiche au-dessus de la page (`<html class="no-js">`, retiré par `app.js`) ; sur mobile, bouton « Fermer »
  (pas « Réduire »), reste de la page `inert` pendant l'ouverture, focus rendu au ☰. Barre du haut : sync, langue,
  thème, déconnexion.
- Thèmes clair / sombre : toutes les couleurs en variables `light-dark(clair, sombre)` dans `app.css` ; par défaut le
  système, bouton (`data-theme-toggle`, `app.js`) → cookie `hangarr_theme` lu par le serveur (`<html data-theme>`, pas de
  flash). Palette **graphite doux, légèrement bleuté** (identité propre, pas de crème/anthracite chaud façon Claude,
  décision utilisateur ; ni blanc ni noir purs, texte ~13:1, bordures discrètes). Boutons `--btn` au-dessus des cartes
  (visibles en sombre), `--on-accent` pour le texte sur fond accent/danger. Couleurs des graphiques validées pour les deux
  surfaces (validateur dataviz lancé dans Docker, surfaces `#f8f9fa` / `#1f2227`). Les pastilles
  posées sur les affiches restent sombres dans les deux thèmes. Style standard : police Inter, retouches limitées à
  `:root:not([data-style])` ; clair adouci (pas de surfaces quasi blanches, décision utilisateur).
- Styles d'interface (Réglages > Apparence, expérimentaux, décision utilisateur) : standard, **Catppuccin** (Latte en
  clair adouci, Macchiato en sombre), **Lumen** (verre liquide sur aurore, palettes Aurore / Océan / Braise / Jade),
  **Y2K** (techno-skeuomorphisme : métal, LCD, voyants, vu-mètres, fenêtres Aqua). Cookies `hangarr_style` /
  `hangarr_palette` (listes blanches `styles` / `palettes` de `lang.go`, routes `GET /style/{name}` et
  `/palette/{name}?next=` comme `/lang`), rendus par le serveur (`<html data-style data-palette>`, pas de flash) ; une
  feuille par style (`static/<style>.css`) chargée seulement si actif, règles limitées à `:root[data-style="…"]`.
  Polices vendorisées dans `static/fonts` (OFL, licences jointes, servies en `font/woff2`). Règle : **une couleur = une
  variable** de `app.css` ; un nouvel élément d'UI qui les utilise s'adapte à tous les styles. Couleurs de graphiques
  des styles optionnels non validées dataviz (d'où « expérimental »).
- Logo : H (couleur `--logo-h`, marine en clair / blanc en sombre, redéfinie par style) + bouton lecture orange en dégradé ;
  template `logo` (`partials/nav.html`, argument = suffixe de l'id du dégradé, unique par page) = même dessin que
  `static/favicon.svg` (suit le thème du système par `<style>` interne, CSP propre aux SVG dans `static()`). PNG
  (`favicon-32.png`, aussi servi sur `/favicon.ico` ; `apple-touch-icon.png`) générés par `make icons`.
- Projets Compose : `hangarr` = `compose.yml` (prod), `hangarr-dev` = `compose.dev.yml`, `hangarr-e2e`,
  `hangarr-deploy-check`, `hangarr-jellyfin-lab` : ne jamais leur donner le même nom (un `down` emporterait l'autre).
- `make e2e` tourne dans le projet compose `hangarr-e2e` (séparé) et lit le mot de passe initial dans les logs.

## Phase 6 : périmètre convenu avec l'utilisateur

En plus de SPEC §15.6 (Santé sans Prowlarr, webhook Tautulli, sélection en lot, image multi-arch, e2e complets ;
**pas d'export CSV**, décision utilisateur) :

- **Déploiement** : Docker (`compose.yml`, `make deploy-check`) et Kubernetes en **manifests YAML simples, sans
  kustomize** (décision utilisateur) : `deploy/kubernetes/hangarr.yaml` (sans namespace imposé ; ConfigMap, PVC, Deployment,
  Service), validé par kubeconform. Même image et mêmes invariants pour les deux (nonroot, rootfs en lecture seule,
  `/data` seul inscriptible, variantes `_FILE`) : ne rien introduire de spécifique à l'un des deux.
  Publication de l'image : GHCR via un workflow Forgejo Actions sur tag, **plus tard** (après la phase 6) ;
  en attendant `make image PUSH=1 IMAGE=ghcr.io/<user>/hangarr`.

- **Configuration des services dans l'UI** (espace « Configuration ») : URL, URL publique, clé d'API / token de Sonarr,
  Radarr, Plex, Tautulli (+ secret du webhook), avec bouton « Tester la connexion ».
  - Secrets **chiffrés en base** (AES-256-GCM, nonce aléatoire par valeur). Clé maître **mode hybride** (décision
    utilisateur) : `HANGARR_MASTER_KEY` / `HANGARR_MASTER_KEY_FILE` (secret Docker / Kubernetes) = mode recommandé ;
    sinon clé générée au premier démarrage dans `DATA_DIR/master.key` (0600), avec avertissement explicite dans l'UI
    (une sauvegarde du volume expose les secrets). Si une clé d'env apparaît alors que `master.key` existe : rechiffrement
    des secrets au démarrage puis suppression du fichier (rien à ressaisir).
  - Champs secrets en écriture seule : jamais renvoyés au navigateur (afficher `••••` + 4 derniers caractères) ;
    champ vide = conserver la valeur actuelle.
  - Variables d'env `SONARR_URL`, `*_API_KEY`… deviennent **optionnelles et prioritaires** : si définies, le champ est
    verrouillé dans l'UI (« défini par l'environnement »). `RequireUpstreams` ne bloque plus le démarrage : sans
    config, l'app tourne, la sync est désactivée et un bandeau invite à configurer.
  - Changement de config : clients reconstruits à chaud, changement journalisé (onglet Sécurité).
- **Bouton « Protéger / Ne plus protéger »** sur la fiche : ajoute/retire le premier tag de `PROTECTED_TAGS` via l'API
  Sonarr/Radarr (PUT de l'objet brut, comme pour le démonitorage), journalisé dans `action_log`, soumis au dry-run.
