# arrstats

Application Go + HTMX auto-hébergée qui croise Tautulli, Plex, Sonarr et Radarr pour savoir ce qui est
regardé, par qui, ce qui ne l'est jamais, et faire le ménage proprement. Spécification complète : [SPEC.md](SPEC.md).

> État : **phase 2** (authentification, synchronisation Sonarr / Radarr / Plex / Tautulli, rapprochement, scoring).
> L'interface (bibliothèque, statistiques) arrive ensuite.

## Prérequis

Uniquement **Docker** (Engine ≥ 24 recommandé) avec **Compose v2** (≥ 2.20 recommandé). Aucun Go, Node, linter ou
navigateur n'est nécessaire sur la machine : tout tourne dans des conteneurs.

## Développement

| Cible `make` | Commande `docker compose` équivalente |
|---|---|
| `make dev` | `docker compose -f compose.dev.yml up --build app mock` |
| `make dev-real` | `ARRSTATS_ENV_FILE=.env docker compose -f compose.dev.yml --profile real up --build app-real` |
| `make test` | `docker compose -f compose.dev.yml --profile tools run --rm test` (après `build test`) |
| `make lint` | `docker compose -f compose.dev.yml --profile tools run --rm lint` (après `build lint`) |
| `make e2e` | voir le `Makefile` : démarre `app-e2e` + `mock`, lit le mot de passe initial dans les logs, lance `e2e`, arrête tout |
| `make deps` | `docker compose -f compose.dev.yml --profile tools run --rm gotools go mod tidy` |
| `make image-load` | `docker build --target runtime -t arrstats:local .` |
| `make image` | `docker buildx build --platform linux/amd64,linux/arm64 --target runtime …` (`PUSH=1` pour pousser) |
| `make reset-admin` | `docker compose -f compose.dev.yml exec app arrstats admin reset-password` |
| `make sync` | `docker compose -f compose.dev.yml exec app arrstats sync --once` |
| `make capture-fixtures` | `ARRSTATS_ENV_FILE=.env docker compose -f compose.dev.yml --profile real run --rm capture` |
| `make clean` | `docker compose -f compose.dev.yml --profile real --profile tools --profile e2e down -v` |

`make dev` lance l'application en hot reload sur http://localhost:8080 contre `mockarr`, de faux
Sonarr/Radarr/Plex/Tautulli. `DRY_RUN=true` est toujours actif par défaut.

Pour travailler contre les vraies instances : `cp .env.example .env`, remplir les URL et les clés, puis `make dev-real`.
Pour valider les structs sur vos vraies réponses : `make capture-fixtures`, qui écrit des réponses anonymisées dans
`testdata/captured/` (à relire avant de commiter) ; `make test` vérifie ensuite qu'elles se décodent.

### Premier démarrage

Au premier démarrage sur une base vide, un compte `admin` est créé et son mot de passe s'affiche **une seule fois**
dans les logs :

```
================================================================
 ARRSTATS : COMPTE ADMIN INITIAL CRÉÉ
 Utilisateur  : admin
 Mot de passe : …
 ...
```

En dev : `docker compose -f compose.dev.yml logs app | grep -A4 "COMPTE ADMIN"`. Le changement est obligatoire à la
première connexion. Mot de passe perdu : `make reset-admin` (ou `arrstats admin reset-password` via
`docker exec` / `kubectl exec`), qui affiche un nouveau mot de passe temporaire uniquement dans la sortie de la commande.

## Récupérer le token Plex et les clés d'API

- **Sonarr / Radarr** : *Settings → General → Security → API Key*.
- **Tautulli** : *Settings → Web Interface → API* (cocher *Enable API*), copier l'*API key*.
- **Plex** : ouvrir un média dans Plex Web → *⋯ → Obtenir des infos → Afficher le XML* ; l'URL contient
  `X-Plex-Token=…`. Voir aussi la documentation officielle « Finding an authentication token / X-Plex-Token ».

## Kubernetes

L'image est distroless, tourne en non-root (UID/GID 65532), fonctionne avec `readOnlyRootFilesystem: true` et
n'écrit que dans `/data` (volume persistant `ReadWriteOnce`, une seule réplique, stratégie `Recreate`).
Sondes : `GET /healthz` (liveness) et `GET /readyz` (readiness). Arrêt propre sur `SIGTERM` (20 s).
Les secrets acceptent une variante `_FILE` (ex. `SONARR_API_KEY_FILE=/run/secrets/arrstats/sonarr-api-key`).

Mot de passe initial : `kubectl -n arrstats logs deploy/arrstats | grep -A4 "COMPTE ADMIN"`.

Les manifests kustomize (`deploy/k8s/`) et `make k8s-validate` arrivent en phase 6.

## Configuration

Toutes les variables sont décrites dans [SPEC.md §10](SPEC.md#10-configuration-variables-denvironnement).
Écarts : `SYNC_INTERVAL` est remplacé par `SYNC_TIME` (heure de la synchronisation quotidienne, défaut `04:00`,
modifiable ensuite dans la page Réglages) ; les variables `PROWLARR_*` n'existent pas.
Ajouts : `SONARR_PUBLIC_URL`, `RADARR_PUBLIC_URL` et `PLEX_PUBLIC_URL` (optionnels) donnent les adresses utilisées par les
liens « Ouvrir dans Sonarr/Radarr/Plex » quand les URL d'API ne sont pas joignables depuis le navigateur (Service
Kubernetes interne). Par défaut : les URL d'API, et app.plex.tv pour Plex.
