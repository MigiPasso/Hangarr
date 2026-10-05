# CLAUDE.md : conventions cleanarr

`SPEC.md` est la spécification de référence (Go + HTMX, croise Plex / Tautulli / Sonarr / Radarr).
**Prowlarr est hors périmètre** (décision utilisateur) : aucun client, aucune config, aucune page ; ignorer SPEC §3.3,
la partie Prowlarr de §7.7 et `PROWLARR_*` de §10.
**Pas de « lectures en cours »** (décision utilisateur) : ignorer `get_activity` (SPEC §3.5) et le bloc « En cours de
lecture » du Dashboard (§7.1).
L'application s'appelle **Cleanarr** (affichage) ; dépôt, binaire, module Go, image et préfixe des variables : `cleanarr` / `CLEANARR_` (renommée depuis « arrstats », décision utilisateur ; aucune compatibilité gardée, rien n'était déployé).

## Règle n°1 : tout passe par Docker

Rien ne s'exécute sur la machine hôte en dehors de `docker`, `docker compose`, `make` et `git`.
**Jamais** de `go`, `gofmt`, `npm`, `npx`, `node`, `golangci-lint`, `playwright` ni de navigateur en local.

| Besoin | Commande |
|---|---|
| Lancer l'app (hot reload + mocks) | `make dev` → http://localhost:8080 (`DEV_PORT=8081 make dev` si le port est pris) |
| Contre les vraies instances | `make dev-real` (lit `.env`, `DRY_RUN=true` par défaut) |
| Tests unitaires et d'intégration (`-race`) | `make test` |
| `go vet` + golangci-lint | `make lint` |
| e2e Playwright (image runtime, base vierge) | `make e2e` |
| `go mod tidy` | `make deps` |
| Autre commande Go ponctuelle | `docker compose -f compose.dev.yml --profile tools run --rm gotools <cmd>` (ex. `gofmt -l -w .`) |
| npm pour e2e | `docker compose -f compose.dev.yml --profile e2e run --rm --no-deps e2e npm …` |
| Télécharger htmx / la liste de mots de passe | `make vendor-assets` |
| Image runtime locale / multi-arch | `make image-load` / `make image` (`PUSH=1` pour pousser) |
| Vérifier le déploiement Docker (`compose.yml`) | `make deploy-check` |
| Lancer une sync dans l'app de dev | `make sync` (`cleanarr sync --once`) |
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
  `internal/i18n/fr.go`, `TestFrenchCatalogComplete` vérifie chaque clé. Langue : cookie `cleanarr_lang` (lien EN / FR,
  `GET /lang/{code}?next=` limité aux chemins locaux), sinon `DEFAULT_LANG` (`en`). Tests web et instances e2e en
  `DEFAULT_LANG=fr`. Nombres, tailles, dates et durées relatives via le `Localizer`. Titres dynamiques : `pageRaw`.
- Aide contextuelle : explications courtes derrière un « i » (`{{template "info" (t "…")}}`, bulle CSS au survol et au
  focus), **toujours à côté** du titre ou du `<label>` (`.title-row`), jamais dedans (sinon le texte d'aide entre dans
  le nom accessible). Explications concises.
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

- `DRY_RUN=true` par défaut ; dry-run et tag de protection (`PROTECTED_TAGS`) vérifiés **côté serveur**.
- Suppression de saison : supprimer les fichiers **puis** démonitorer la saison (`PUT` de la série complète).
- **Tautulli est la source de vérité** du visionnage (lectures, vu / pas vu, par utilisateur). Ne jamais utiliser
  `viewCount` / `lastViewedAt` / `viewedLeafCount` de Plex pour ça : ils ne concernent que le compte du token.
- Auth (SPEC §11) : admin créé une seule fois au premier démarrage, mot de passe affiché une seule fois sur stdout,
  changement forcé, argon2id (PHC), sessions stockées hachées (SHA-256), verrouillage progressif, rate limit par IP,
  `X-Forwarded-For` seulement depuis `TRUSTED_PROXIES`.
- Requêtes sortantes : clients amont via `httpx` uniquement (redirections suivies seulement vers le même hôte,
  `httpx.SameHostOnly`, sinon la clé partirait ailleurs) ; images distantes : https seulement, redirections https, 3 max.
  Jamais `http.DefaultClient` pour un appel authentifié.
- Rate limit par IP partagé entre connexion et changement de mot de passe ; CSP avec `object-src 'none'; base-uri 'none'`.
- Kubernetes : image distroless nonroot (65532), rootfs en lecture seule, seul `/data` est inscriptible,
  `/healthz` (liveness) et `/readyz` (readiness, sans dépendre des *arr*), arrêt propre sur SIGTERM (20 s).
  Une seule réplique (SQLite).

## Repères

- `cmd/cleanarr` : `serve`, `sync --once`, `admin reset-password`, `master-key`, `backup [PATH]`, `healthcheck`, `version`.
- `internal/mockarr` (servi par `cmd/mockarr`) : faux upstreams (sonarr :8989, radarr :7878, plex :32400,
  tautulli :8181) depuis `testdata/`, appels destructeurs exposés sur `GET /_calls`, `POST /_reset`.
  Les tests d'intégration l'utilisent via `httptest`. `MOCK_SHIFT_DATES=true` recale l'historique sur aujourd'hui.
- `testdata/` : scénario synthétique décrit dans `testdata/README.md` (les tests en dépendent) ;
  `testdata/captured/` : vraies réponses anonymisées, seulement vérifiées au décodage.
- `internal/syncer` (et non `internal/sync`, pour ne pas masquer le paquet standard) : fetch parallèle, stockage,
  rapprochement (`internal/matching`), liaison de l'historique, recalcul (`internal/scoring`).
- Saison 0 (spéciaux) exclue de la progression et de `watch_state`.
- Bibliothèque : une requête SQL paramétrée par lot (`store.LibraryQuery`, CTE `v` avec colonnes `eff_*` ; point de vue
  utilisateur calculé à la volée, mêmes règles que `scoring`). Recherche sur `title_norm` (sans accents ni article).
  Recherche déclenchée par `input` (et non `keyup` comme dans la spec) pour couvrir collage, autocomplétion, bouton ✕.
- Sync : **une fois par jour** à l'heure réglée dans l'UI (page Réglages, table `app_settings`, défaut `SYNC_TIME`=04:00,
  fuseau `TZ`) + rattrapage au démarrage si la dernière sync a plus de 24 h. `SYNC_INTERVAL` de la spec est supprimé.
- Suppressions : `internal/actions` (tag de protection relu en live sur Sonarr/Radarr, dry-run, titre à saisir pour une
  série, journal `action_log` pour toute tentative). Saison : fichiers (`DELETE episodefile/bulk`) PUIS `PUT` de l'objet
  série **brut** avec `monitored=false` (`sonarr.UnmonitorSeason`). Resync ciblée ensuite (`syncer.RefreshSeries`).
- Fiche détail : progression exacte via `GET /api/v3/episode?seriesId=` (appel live, 5 s max, repli approximatif).
  Liens : `SONARR_PUBLIC_URL`, `RADARR_PUBLIC_URL`, `PLEX_PUBLIC_URL` (optionnels ; Plex par défaut via app.plex.tv).
- Phase 5 : Dashboard (sans lectures en cours), Stats (/stats, /stats/users/{id}), Nettoyage (/cleanup). Graphiques SVG
  générés en Go (`internal/web/charts.go`), palette validée (skill dataviz) : bleu #3987e5, orange #d95926, rampe bleue
  pour la heatmap ; chaque graphique a une info-bulle `<title>` et un tableau de données. **Pas de score de nettoyage**
  (décision utilisateur, remplace SPEC §7.5) : candidat = géré par Sonarr/Radarr, avec fichiers, non protégé, et personne
  ne l'a regardé depuis N jours (jamais vu : depuis `added_at`), N réglable dans Réglages (`cleanup_inactive_days`,
  défaut 180). Règle évaluée en SQL (`store.CleanupScope`), liste triée par taille avec la raison en clair.
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
- Santé (`/health`) : services vérifiés en direct (`upstream.CheckAll`, chargés en différé par HTMX, alertes
  `/api/v3/health` de Sonarr/Radarr), derniers passages et historique de `sync_runs`, écarts de données
  (`store.DataHealth`).
- Webhook Tautulli (`POST /webhooks/tautulli`, seule route sans session) : 404 sans secret, `X-Cleanarr-Token` comparé
  en temps constant, corps ignoré, 60 req/min/IP, puis `syncer.RequestHistorySync` (anti-rebond 30 s, historique seul,
  partage le verrou de la sync complète). `LastSyncAt` ne compte que les sources de catalogue. Secret généré dans
  Configuration (affiché une seule fois).
- `internal/images` : proxy MediaCover (`poster-500.jpg`) → cache disque `DATA_DIR/cache/img`, ETag, sémaphore (4),
  singleflight, repli `remoteUrl` https, placeholder SVG. Pas de JS supplémentaire côté client.
- Sélection en lot (bibliothèque grille/tableau + Nettoyage) : cases `name=ids form=batch-form` (absentes si protégé ou
  Plex seul), barre collante `partials/batch.html`, `POST /batch/delete` (1er POST = confirmation, `step=run` = exécution ;
  série dans le lot + `CONFIRM_BY_TYPING` → saisir le nombre d'éléments), `POST /batch/protect` (sans confirmation).
  `actions.DeleteBatch/ProtectBatch` : séquentiel, `Delete` normal par élément (`Confirmed`, `SkipPlexRefresh`), un seul
  rafraîchissement Plex par type à la fin, 100 éléments max, délai d'écriture prolongé (10 min). Rapport par élément dans la
  modale `#confirm`, événement `cleanarr:batch-done` (cartes supprimées retirées, rechargement à la fermeture après protection).
- Déploiement Docker : `compose.yml` (prod, projet `cleanarr`, volume `cleanarr-data`, durci comme k8s, `.env` facultatif),
  `make deploy-check` = `compose.yml` tel quel + `deploy/compose.check.yml` (mock, sans port ni `.env`) + `deploy/check.sh`.
  `cleanarr backup [PATH]` (`VACUUM INTO`, à chaud). Avertissement sur la page de connexion si HTTP avec `COOKIE_SECURE=true`
  (hors localhost). Compose ≥ 2.24 (`env_file.required`, `!reset`).
- Thèmes clair / sombre : toutes les couleurs en variables `light-dark(clair, sombre)` dans `app.css` ; par défaut le
  système, bouton (`data-theme-toggle`, `app.js`) → cookie `cleanarr_theme` lu par le serveur (`<html data-theme>`, pas de
  flash). Couleurs des graphiques validées pour les deux surfaces (validateur dataviz lancé dans Docker). Les pastilles
  posées sur les affiches restent sombres dans les deux thèmes.
- Projets Compose : `cleanarr` = `compose.yml` (prod), `cleanarr-dev` = `compose.dev.yml`, `cleanarr-e2e`,
  `cleanarr-deploy-check` : ne jamais leur donner le même nom (un `down` emporterait l'autre).
- `make e2e` tourne dans le projet compose `cleanarr-e2e` (séparé) et lit le mot de passe initial dans les logs.

## Phase 6 : périmètre convenu avec l'utilisateur

En plus de SPEC §15.6 (Santé sans Prowlarr, webhook Tautulli, sélection en lot, image multi-arch, e2e complets ;
**pas d'export CSV**, décision utilisateur) :

- **Déploiement** : Docker (`compose.yml`, `make deploy-check`) et Kubernetes en **manifests YAML simples, sans
  kustomize** (décision utilisateur) : `deploy/kubernetes/cleanarr.yaml` (sans namespace imposé ; ConfigMap, PVC, Deployment,
  Service), validé par kubeconform. Même image et mêmes invariants pour les deux (nonroot, rootfs en lecture seule,
  `/data` seul inscriptible, variantes `_FILE`) : ne rien introduire de spécifique à l'un des deux.
  Publication de l'image : GHCR via un workflow Forgejo Actions sur tag, **plus tard** (après la phase 6) ;
  en attendant `make image PUSH=1 IMAGE=ghcr.io/<user>/cleanarr`.

- **Configuration des services dans l'UI** (espace « Configuration ») : URL, URL publique, clé d'API / token de Sonarr,
  Radarr, Plex, Tautulli (+ secret du webhook), avec bouton « Tester la connexion ».
  - Secrets **chiffrés en base** (AES-256-GCM, nonce aléatoire par valeur). Clé maître **mode hybride** (décision
    utilisateur) : `CLEANARR_MASTER_KEY` / `CLEANARR_MASTER_KEY_FILE` (secret Docker / Kubernetes) = mode recommandé ;
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
