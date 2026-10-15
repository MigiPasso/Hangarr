<p align="center"><img src="internal/web/static/favicon.svg" alt="Hangarr logo" width="96" height="96"></p>

# Hangarr

Hangarr is a self-hosted web app (Go and HTMX) that cross-references Sonarr, Radarr, and Plex (with optional Tautulli) or Jellyfin. It shows what is watched and by whom, what is never watched, what is coming up, and whether the stack is healthy. It also helps you clean up safely: deletions are simulated until you turn dry-run off.

## Features

- **Dashboard**: space used and reclaimable, hours watched, most watched and recently watched titles.
- **Library** (series and movies): poster grid or table, search, infinite scrolling. Filters answer one question each: *Who?* (everyone, or one viewer), *Watch state*, *Present in* (media server and/or Sonarr/Radarr), *Not watched for*, *Added*, *Size (GB)*, *Status*. Genres, year, rating, tags, quality profile and network or studio are under *More filters*.
- **Detail page**: progress per season and per viewer, play history, and links to Sonarr, Radarr and the media server.
- **Upcoming**: release calendar of monitored items (Sonarr episodes; Radarr cinema, digital and physical releases), as a list, a poster grid or a month view.
- **Cleanup**: items and seasons nobody has watched for N days (180 by default), sorted by size with the reason.
- **Duplicates**: movies the media server holds more than once, with their versions and tracks. **Keep all** hides a group until its versions change; a single version can be deleted through the media server.
- **Deletions** through Sonarr and Radarr (series, season, movie), one at a time or in batches. Items that only the media server knows are deleted through it, files included (Plex requires *Allow media deletion* to be enabled).
- **Protection**: a Sonarr/Radarr tag (the first of `PROTECTED_TAGS`), or a Hangarr-only protection for items that only the media server knows. Protected items are never proposed nor deleted.
- **Safeguards**: `DRY_RUN=true` by default, tags re-read live before each action, a title to type for a whole series, and a log of every attempt.
- **Stats** per period and per viewer, **Health** (services checked live, sync history, data gaps) and **Log** (actions and security events).
- **Synchronization** once a day, or every 1, 2, 3, 4, 6, 8 or 12 hours from a chosen start time, plus **Sync now**. An optional Tautulli webhook refreshes the history after each play.
- Interface in English or French; light and dark themes; three experimental styles (Catppuccin, Lumen, Y2K) in **Settings › General**.

## Status

- Version 1.x. Each release is a git tag `vX.Y.Z`.
- Single-maintainer project.
- The database schema moves forward automatically at startup; there is no downgrade. To return to an older version, restore a backup taken before the upgrade (see [Backup](docs/deployment.md#8-operations)).
- `DRY_RUN=true` by default: nothing is deleted until you change it.

## Requirements

To run Hangarr:

- Docker Engine 24 or later (recommended) with Docker Compose 2.24 or later, or a Kubernetes cluster. The image builds for `linux/amd64` and `linux/arm64`.
- Sonarr and Radarr (API v3).
- Plex Media Server, with Tautulli (API v2) for play durations and devices; or Jellyfin (version `12.2.20261005-225228` is the one pinned in the test lab, [`deploy/jellyfin-lab/compose.yml`](deploy/jellyfin-lab/compose.yml)). Emby is not supported. Hangarr reads one media server at a time.

To develop: Docker, Docker Compose, `make` and `git`. Nothing else is installed on the host.

## Quick start

With Docker:

```sh
git clone <repository-url> hangarr
cd hangarr
cp .env.example .env                                     # optional: DRY_RUN, COOKIE_SECURE, TZ…
docker compose up -d --build
docker compose logs hangarr | grep -A4 "INITIAL ADMIN"   # one-time admin password
```

1. Open http://localhost:8080 and sign in as `admin` with the password from the logs. It is shown only once, and you must change it at the first sign-in. To choose the password instead, set `HANGARR_ADMIN_PASSWORD` in `.env` before the first start (see [docs/configuration.md](docs/configuration.md#admin-password)).
2. In **Settings › Services**, enter the address and key of Sonarr, Radarr, Plex or Jellyfin, and Tautulli if you use it. Click **Test connection**, then **Save**. The first sync starts once the configuration is complete.
3. Keep `DRY_RUN=true` until you have reviewed the cleanup proposals.

Over plain HTTP on a LAN address, set `COOKIE_SECURE=false` in `.env`, otherwise the browser refuses the session cookie. This is not needed on `localhost`. Behind an HTTPS reverse proxy, keep `COOKIE_SECURE=true` and set `TRUSTED_PROXIES`.

For Kubernetes and the complete guide, see [docs/deployment.md](docs/deployment.md).

## Configuration

Hangarr is configured with environment variables, and with **Settings › Services** for the services. The full list is in [docs/configuration.md](docs/configuration.md#environment-variables).

- **Services**: set them in the interface or in the environment (`SONARR_URL`, `SONARR_API_KEY`, `PLEX_TOKEN`…). A value set in the environment takes precedence and locks its field.
- **Master key** (recommended): saved keys and tokens are encrypted in the database. Provide the key through `HANGARR_MASTER_KEY_FILE`, for example a Docker or Kubernetes secret. Without it, a key is generated in `DATA_DIR/master.key`, inside the data volume. Generate one with `docker run --rm hangarr:local master-key`.
- **One media server**: Plex or Jellyfin, never both. Tautulli is only used with Plex. Plex and Jellyfin variables set together stop the application at startup.
- **Schedule and cleanup**: the sync time and frequency, and the number of days of the cleanup rule, are set in **Settings › General**.

## Usage

A typical routine:

1. Let the scheduled sync run, or click **Sync now** in the top bar.
2. Explore the **Dashboard** and the **Library**, and use the filters to find what nobody watches.
3. Review **Cleanup** while `DRY_RUN=true`. Simulated deletions are recorded in **Log**.
4. Protect what must stay: **Protect** on an item's page adds the first tag of `PROTECTED_TAGS` (`keep` by default). Check **Duplicates** for movies stored twice.
5. When the proposals are right, set `DRY_RUN=false` in `.env` and run `docker compose up -d`. Deletions then go through Sonarr, Radarr or the media server, after confirmation (a whole series requires its title to be typed).

Command-line subcommands, run in the container with `docker compose exec hangarr /hangarr <command>` (on Kubernetes: `kubectl -n <ns> exec deploy/hangarr -- /hangarr <command>`):

| Command | Description |
|---|---|
| `serve` | start the web server (default) |
| `sync --once` | run one synchronization and print a summary |
| `admin reset-password [--username NAME]` | print a new temporary password for an account |
| `backup [PATH]` | write a consistent copy of the database, safe while serving |
| `master-key` | print a new random master key, for `HANGARR_MASTER_KEY` |
| `healthcheck` | exit 0 if the local server answers `/healthz` |
| `version` | print the version |

## Development

Everything runs in Docker: no Go, Node.js, linter or browser is needed on the host.

- `make dev`: the app with hot reload on http://localhost:8080, against mock Sonarr, Radarr and Plex with Tautulli. Sign in as `admin` / `Hangarr-Dev-1` (development only). Use `DEV_PORT=8081 make dev` if port 8080 is taken, and `MEDIA=jellyfin make dev` to use the mock Jellyfin instead of Plex.
- `make dev-real`: the app against your real services, configured in `.env` (copy `.env.example`). `DRY_RUN=true` by default.
- `make verify`: gofmt, lint and tests, with a compact summary; full logs are in `.verify/`. `make verify E2E=1` also runs the Playwright end-to-end suite.
- `make help`: lists all the targets.

GitHub Actions ([ci.yml](.github/workflows/ci.yml)) runs the same make targets on every push to `main` and every pull request. Pushing a `v*` tag publishes the image to `ghcr.io/migipasso/hangarr` ([release.yml](.github/workflows/release.yml)).

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow, the conventions and the pull request checklist. Report security issues privately rather than in a public issue.

## Documentation

- [docs/deployment.md](docs/deployment.md): Docker and Kubernetes deployment, connecting the services, operations, troubleshooting.
- [docs/configuration.md](docs/configuration.md): environment variables, master key, services, history sources, Tautulli webhook.
- [docs/architecture.md](docs/architecture.md): components, sync pipeline, data model, security model.
- [CONTRIBUTING.md](CONTRIBUTING.md): development workflow and conventions.
- [testdata/README.md](testdata/README.md): the synthetic scenario served by the mock services.
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md): vendored files, fonts, container images and their licenses.
- [LICENSE](LICENSE): MIT License of the project.

## Limitations

- One media server at a time: Plex or Jellyfin.
- One replica only: the database is SQLite on a single volume.
- A single administrator account, with no user management in the interface.
- No Emby and no Prowlarr.
- Duplicates are detected for movies only.
- Jellyfin keeps only the last play of each item for each user, without durations. There is no Jellyfin webhook: its history follows the scheduled sync or **Sync now**.
- The interface is available in English and French only.

## Formerly Cleanarr

Hangarr was previously named Cleanarr. The former variable names, webhook header and database file name are still accepted: see [Deprecated names](docs/configuration.md#deprecated-names).

## License

Hangarr is released under the [MIT License](LICENSE). Third-party components keep their own licenses: see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
