<p align="center"><img src="internal/web/static/favicon.svg" alt="Hangarr logo" width="96" height="96"></p>

# Hangarr

Self-hosted control tower (Go + HTMX) for an *arr* stack: it cross-references Tautulli, Plex or Jellyfin, Sonarr and
Radarr to tell what is watched, by whom, what never is, what is coming up soon, whether everything is fine, and to
clean up properly. Like a hangar, it stores a bit of everything. Full specification: [SPEC.md](SPEC.md).

## Features

- **Dashboard**: volumes, reclaimable space, viewing time, latest plays.
- **Series / Movies**: poster grid or table, search, infinite scrolling. Filters following "one question = one
  filter": *Who?* (everyone, or one viewer: states and last views then become theirs), *State*, *Not watched for*
  (never watched: since it was added), *Added*, *Size*, *Status*, *Present in* (Plex or Jellyfin, and/or Sonarr/Radarr);
  genres, year, rating, tags, profile and network/studio under "More filters".
- **Detail page**: progress per season and per viewer, history, links to Sonarr, Radarr and Plex or Jellyfin.
- **Upcoming**: release calendar of what is monitored (Sonarr episodes, Radarr cinema / digital / physical releases),
  read live, as a list or a paginated poster grid (30 days, 3 months or 1 year; 10, 25, 50 or 100 per page), or as a
  monthly calendar; episodes released the same day are grouped, premieres and finales flagged.
- **Cleanup**: what nobody has watched for N days (adjustable), sorted by size with the reason, filterable by type,
  state, plays, size and age; seasons never watched in series watched elsewhere.
- **Deletions** through Sonarr/Radarr (series, season, movie; for a season, files first, then unmonitoring), one at a
  time or in batches. An item that **only the media server knows** (Plex or Jellyfin, removed from Sonarr/Radarr) is
  deleted by that server, files included. On Plex, enable *Settings › Library › Allow media deletion*; on Jellyfin,
  deletion goes through the API key (a refusal from Jellyfin is reported).
- **Protection**: Sonarr/Radarr tag (the first of `PROTECTED_TAGS`); for an item that only the media server knows,
  protection is kept in Hangarr only (nothing is sent to the server). A protected item is never proposed nor deleted.
- Server-side safeguards: `DRY_RUN=true` by default (everything is simulated and logged), tag re-read live, a title to
  type in for a whole series, a log of every attempt.
- **Stats** (per period and per viewer), **Health** (services checked live, sync history, data gaps), **Log** (actions
  and security).
- **Synchronization** scheduled in Settings: once a day, or every 12, 8, 6, 4, 3, 2 or 1 h from a chosen time, plus the
  "Sync now" button and the Tautulli webhook.
- Interface in English or French, light and dark themes.

## Requirements

Only **Docker** (Engine ≥ 24 recommended, Docker Desktop or Colima) with **Compose v2** (≥ 2.24) and **Buildx** for the
multi-arch image. No Go, Node, linter or browser is needed on the machine: everything runs in containers.

## Development

| `make` target | Equivalent `docker compose` command |
|---|---|
| `make dev` | `docker compose -f compose.dev.yml up --build app mock` |
| `make dev-real` | `HANGARR_ENV_FILE=.env docker compose -f compose.dev.yml --profile real up --build app-real` |
| `make test` | `docker compose -f compose.dev.yml --profile tools run --rm test` (after `build test`) |
| `make lint` | `docker compose -f compose.dev.yml --profile tools run --rm lint` (after `build lint`) |
| `make e2e` | see the `Makefile`: starts `app-e2e` + `mock`, reads the initial password from the logs, runs `e2e`, stops everything |
| `make deps` | `docker compose -f compose.dev.yml --profile tools run --rm gotools go mod tidy` |
| `make image-load` | `docker build --target runtime -t hangarr:local .` |
| `make image` | `docker buildx build --platform linux/amd64,linux/arm64 --target runtime …` (`PUSH=1` to push) |
| `make reset-admin` | `docker compose -f compose.dev.yml exec app hangarr admin reset-password` |
| `make sync` | `docker compose -f compose.dev.yml exec app hangarr sync --once` |
| `make deploy-check` | validates `compose.yml`, then starts it as is against the mock (separate project) and runs `deploy/check.sh` |
| `make capture-fixtures` | `HANGARR_ENV_FILE=.env docker compose -f compose.dev.yml --profile real run --rm capture` |
| `make clean` | `docker compose -f compose.dev.yml --profile real --profile tools --profile e2e down -v` |

`make dev` runs the application with hot reload on http://localhost:8080 against `mockarr`, fake
Sonarr/Radarr/Plex/Jellyfin/Tautulli. `DRY_RUN=true` is always active by default.

`MEDIA` chooses the media server of this instance: `MEDIA=jellyfin make dev` (the mock's Jellyfin, without Plex or
Tautulli) or `MEDIA=none make dev` (no server: Plex or Jellyfin is then configured in **Settings › Services**).
Without `MEDIA`, the mock's Plex and Tautulli are used.

To work against the real instances: `cp .env.example .env`, fill in the URLs and keys, then `make dev-real`.
To validate the structs against your real responses: `make capture-fixtures`, which writes anonymized responses to
`testdata/captured/` (ignored by git, never committed); `make test` then checks that they decode.

### First start

On the first start with an empty database, an `admin` account is created and its password is shown **only once** in
the logs:

```
================================================================
 HANGARR: INITIAL ADMIN ACCOUNT CREATED
 Username : admin
 Password : …
 ...
```

Changing the password is mandatory at the first sign-in.

To choose the password in advance: `HANGARR_ADMIN_PASSWORD` (at least 12 characters, lowercase, uppercase, digit and
symbol, without the username; otherwise the application refuses to start). It is then neither displayed nor required
to change. The variable is only used to create the account: afterwards it is ignored (warning in the logs) and can be
removed.

In dev, `compose.dev.yml` sets it: sign in directly with **`admin` / `Hangarr-Dev-1`** on a new database (after
`make clean`). Lost password: `make reset-admin` (or `hangarr admin reset-password` via `docker exec` / `kubectl exec`),
which displays a new temporary password only in the command output.

## Getting the Plex token and the API keys

- **Sonarr / Radarr**: *Settings → General → Security → API Key*.
- **Tautulli**: *Settings → Web Interface → API* (tick *Enable API*), copy the *API key*.
- **Plex**: open a media item in Plex Web → *⋯ → Get Info → View XML*; the URL contains `X-Plex-Token=…`. See also the
  official documentation "Finding an authentication token / X-Plex-Token". Use the server owner's token (needed for the
  history of all accounts and for deletions through Plex).
- **Jellyfin**: *Dashboard → API Keys → +*, then copy the key.

## Formerly Cleanarr

Hangarr was called Cleanarr. An existing installation is migrated without re-entering anything (database renamed at
startup, old `CLEANARR_MASTER_KEY*` variables and `X-Cleanarr-Token` header still accepted): see
[docs/deploiement.md, "Migrate from Cleanarr"](docs/deploiement.md#11-migrer-depuis-cleanarr) (in French).

## Deployment

Full guide, from publishing the image to connecting Sonarr, Radarr, Plex (or Jellyfin) and Tautulli, with
troubleshooting: [docs/deploiement.md](docs/deploiement.md) (in French). The sections below are its essentials.

### Publishing a release

From an up-to-date `main`, on a clean tree (otherwise the version ends with `-dirty`):

```sh
git tag vX.Y.Z && git push origin vX.Y.Z
echo "ghp_…" | docker login ghcr.io -u <user> --password-stdin   # classic token, scope write:packages
make image PUSH=1 IMAGE=ghcr.io/<user>/hangarr                  # amd64 + arm64, tag = git describe
docker buildx imagetools inspect ghcr.io/<user>/hangarr:vX.Y.Z  # two platforms expected
```

Only the version tag is pushed. To keep `:latest` up to date as well:
`docker buildx imagetools create -t ghcr.io/<user>/hangarr:latest ghcr.io/<user>/hangarr:vX.Y.Z`.
Details and troubleshooting: [docs/deploiement.md](docs/deploiement.md#2-publier-limage-sur-ghcr) (in French).

### Deploying with Docker

The [`compose.yml`](compose.yml) file is ready to use: hardened image (distroless, user 65532, read-only file system,
no capabilities), a volume for `/data`, automatic restart, built-in healthcheck.

```sh
cp .env.example .env            # optional: DRY_RUN, COOKIE_SECURE, TZ…
docker compose up -d --build
docker compose logs hangarr | grep -A4 "INITIAL ADMIN"   # initial password, shown only once
# or set HANGARR_ADMIN_PASSWORD in .env before the first start (see "First start")
```

Open `http://<host>:8080`, change the password, then configure Sonarr, Radarr, Plex or Jellyfin, and Tautulli (Plex only)
in **Settings › Services** (**Réglages › Configuration** in the French interface), with the "Test connection" button.
The first sync starts as soon as the configuration is complete.

- **Image and port**: `HANGARR_IMAGE` (default: built locally from the sources, `hangarr:local`) and `HANGARR_PORT`
  (default `8080`) in `.env`.
- **Without HTTPS** (testing on the local network): set `COOKIE_SECURE=false` in `.env`, otherwise the browser refuses
  the session cookie (the sign-in page says so). Behind an HTTPS reverse proxy, set it back to `true` and declare the
  proxy's address in `TRUSTED_PROXIES`.
- **Same network as the *arr* stack**: uncomment the `networks` block of `compose.yml` (network name: `docker network
  ls`). Sonarr is then reached at `http://sonarr:8989`, and Tautulli calls the webhook at
  `http://hangarr:8080/webhooks/tautulli`.
- **Master key (recommended)**: `docker run --rm <image> master-key > master.key` (`hangarr:local`, or the published
  image), then uncomment the `secrets` blocks of `compose.yml`. Without it, the key is generated in the volume (see
  "Service configuration").
- **Equivalent `docker run`**:
  ```sh
  docker run -d --name hangarr --restart unless-stopped -p 8080:8080 --env-file .env \
    -v hangarr-data:/data --read-only --user 65532:65532 --cap-drop ALL \
    --security-opt no-new-privileges:true hangarr:local
  ```
- **Host folder instead of a volume** (`-v /srv/hangarr:/data`): it must belong to the container user:
  `sudo chown -R 65532:65532 /srv/hangarr`.
- **Update**: `git pull && docker compose up -d --build`, or with the published image: put
  `HANGARR_IMAGE=ghcr.io/<user>/hangarr:<version>` **in `.env`**, then `docker compose pull && docker compose up -d`.
  Database migrations run at startup.
- **Backup**, hot: `docker compose exec hangarr /hangarr backup` writes a consistent copy to
  `/data/backups/hangarr-YYYYMMDD-HHMMSS.db` (or `… backup /data/backups/my-file.db`). Saved API keys are encrypted
  there: keep the master key too (`master.key` in the volume, or `HANGARR_MASTER_KEY`). To restore: stop the container,
  replace `/data/hangarr.db` with the copy, start it again.
- **Lost admin password**: `docker compose exec hangarr /hangarr admin reset-password`.

### Kubernetes

Plain YAML manifests in [`deploy/kubernetes/hangarr.yaml`](deploy/kubernetes/hangarr.yaml), with no namespace set (the
one of `kubectl -n` or of your Argo CD application): ConfigMap, PVC, Deployment (one replica, `Recreate` strategy,
non-root, read-only root filesystem, `/healthz` and `/readyz` probes) and Service. The steps are at the top of the file:

1. publish the image to a registry the nodes can pull from (see "Publishing a release" above) and set `image:`;
2. create the master key secret: `kubectl -n <ns> create secret generic hangarr-master-key
   --from-literal=master-key="$(openssl rand -base64 32)"`;
3. `kubectl -n <ns> apply -f deploy/kubernetes/hangarr.yaml` (or Argo CD sync), then
   `kubectl -n <ns> logs deploy/hangarr | grep -A4 "INITIAL ADMIN"` for the initial password;
4. configure the services in **Settings › Services** with their in-cluster addresses
   (`http://sonarr.<namespace>.svc.cluster.local:8989`…);
5. open the app: `kubectl -n <ns> port-forward svc/hangarr 8080:8080` then `http://localhost:8080`, or expose the
   Service (Ingress; example with Tailscale in the guide). Over HTTPS or `port-forward`, keep `COOKIE_SECURE: "true"`
   in the ConfigMap; set `TRUSTED_PROXIES` to the CIDR of the ingress controller pods.

For the image, see "Publishing a release" above. A GHCR package is private by default: make it public, or create a
`docker-registry` secret and uncomment `imagePullSecrets`.

Backup and lost admin password: `kubectl -n <ns> exec deploy/hangarr -- /hangarr backup` (copy in `/data/backups/`) and
`kubectl -n <ns> exec deploy/hangarr -- /hangarr admin reset-password`.

## Language

The interface is in English by default, and in French with the **FR** link in the top bar (the choice is kept by the
browser). `DEFAULT_LANG=fr` starts an instance in French.

## Service configuration

Sonarr, Radarr, Plex (or Jellyfin) and Tautulli are configured on the **Settings › Services** page (URL, public URL,
API key or token, "Test connection" button). Without configuration, the application still starts: synchronization is
disabled and a banner invites you to configure it. The first synchronization starts as soon as the configuration is
complete.

- **Master key**: keys and tokens are encrypted in the database (AES-256-GCM).
  - *Recommended*: provide a 32-byte key with `HANGARR_MASTER_KEY` or `HANGARR_MASTER_KEY_FILE` (Docker or Kubernetes
    secret), kept outside the data volume. To generate one: `docker run --rm <image> master-key`.
  - *Default*: without a provided key, one is generated on the first start in `/data/master.key`. Nothing to prepare,
    but a backup or copy of the volume then exposes the secrets; the Settings › Services page flags it.
  - Switching from the default mode to the recommended one: set the variable and restart. The secrets are re-encrypted
    with the new key and `master.key` is deleted, without re-entering anything. Changing a provided key afterwards,
    however, makes the secrets unreadable: they are flagged and must be re-entered.
- **Environment variables take precedence**: `SONARR_URL`, `SONARR_API_KEY`, `PLEX_TOKEN`… are still accepted. A value
  set in the environment is used as is and locked in the UI.
- Secrets are never sent back to the browser (only `••••` and the last 4 characters are shown). A field left empty
  keeps the current value. Each change is written to the log (Security tab).

### Plex or Jellyfin

Hangarr reads one media server at a time: Plex or Jellyfin. Tautulli and `HISTORY_SOURCE` only concern Plex: with
Jellyfin, Tautulli is not needed and `HISTORY_SOURCE` is ignored.

- **Choice**: configure Plex **or** Jellyfin, not both; the one that is configured is used. On **Settings › Services**,
  the other one's card is greyed out ("Plex is configured: clear it to use Jellyfin."); Tautulli is only used with Plex,
  so its card is greyed out with Jellyfin. Clearing a value is always possible, and the server also checks the choice:
  it refuses to save. To switch servers, clear the configuration of one, then configure the other. Setting `PLEX_*`
  and `JELLYFIN_*` together in the environment prevents startup ("PLEX_* and JELLYFIN_* are both set…").
  If both end up configured anyway (one from the environment, the other in the interface), nothing is synchronized and
  no deletion goes through the server until one of them is cleared; a banner says so. The media server in use is shown
  by a badge in the top bar.
- **Other server's data**: nothing is deleted. Items that only the other server knows are hidden (neither in the
  library nor deletable), their local protections are kept, and they come back when it is used again. The history
  already watched stays in the statistics.
- **No double counting**: when the watched state moves from one server to the other (a sync tool between Plex and
  Jellyfin, or a migration), a Jellyfin play of the same item (same episode) by the same viewer, dated from 30 min
  before to 24 h after a Plex/Tautulli play, is the same play: only the Plex/Tautulli one is kept (it has the
  durations), and it becomes "watched" if Jellyfin says so and no other Plex play explains it. Items marked as watched
  in bulk (at least 20 in the same minute, typical of a migration dated the day of the import) are matched with Plex
  plays whatever their date. A genuine rewatch on the other server within those 24 hours is therefore absorbed.
- **Viewers**: Jellyfin users are matched by name to existing viewers; an unknown name creates a new viewer.
- **Jellyfin history**: it comes from Jellyfin itself. Jellyfin only keeps the last play of each item per user, without
  durations: a replayed item counts once. Plays started but not finished are listed, with their progress.
- **Synchronization**: there is no Jellyfin webhook yet. The history follows the scheduled sync or the "Sync now" button.

## Tautulli webhook (recommended)

Without a webhook, the statistics follow the scheduled sync. With one, they are up to date about 30 seconds after each
playback ends: Tautulli notifies Hangarr, which only re-reads the history.

1. **Settings › Services** → Tautulli → "Generate a secret" (shown only once), or `TAUTULLI_WEBHOOK_SECRET` (32
   characters min.).
2. Tautulli → Settings → Notification Agents → Webhook: URL `http://<hangarr>:8080/webhooks/tautulli`, method `POST`,
   triggers "Playback Stop" and "Watched", and in the Data tab, *JSON Headers*:
   `{"X-Hangarr-Token": "<secret>"}` for both triggers.

The content sent by Tautulli is ignored (the call only triggers reading the history through the API); without a secret,
the route answers 404.

## Environment variables

All variables are described in [SPEC.md §10](SPEC.md#10-configuration-variables-denvironnement). Differences:

- no cleanup score nor `GRACE_DAYS`: the Cleanup page proposes what nobody has watched for N days (never watched: since
  it was added), N adjustable in Settings (180 by default);
- `SYNC_INTERVAL` is replaced by `SYNC_TIME` (time of the sync, default `04:00`); the time and frequency are then set in
  Settings;
- no Prowlarr: the `PROWLARR_*` variables do not exist.

Additions: `SONARR_PUBLIC_URL`, `RADARR_PUBLIC_URL`, `PLEX_PUBLIC_URL` and `JELLYFIN_PUBLIC_URL` (optional) give the
addresses used by the "Open in Sonarr/Radarr/Plex/Jellyfin" links when the API URLs are not reachable from the browser
(internal Kubernetes Service). Default: the API URLs, and app.plex.tv for Plex.

Jellyfin is configured with `JELLYFIN_URL`, `JELLYFIN_API_KEY` (or `JELLYFIN_API_KEY_FILE`), instead of Plex (see
"Plex or Jellyfin"). `PLEX_REFRESH_AFTER_DELETE` (`true` by default) asks the server in use to rescan after a file
deletion: Plex, or all Jellyfin libraries.
