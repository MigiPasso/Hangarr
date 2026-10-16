# Architecture

Hangarr is a single Go binary. `hangarr serve` runs the web interface and the synchronization loop in one process and keeps its state in one SQLite database. This document explains how the parts fit together. Installation and configuration are described in the [deployment guide](deployment.md) and the [configuration reference](configuration.md).

## Overview

```text
Browser
  |  HTTP: pages, HTMX fragments, /img/... (images are proxied)
  v
hangarr serve (one process)
  |-- internal/web        routing, sessions, pages, SVG charts, image proxy
  |-- internal/actions    deletions and protection, behind dry-run and safeguards
  |-- internal/syncer     scheduled and on-demand synchronization
  |     |--> Sonarr                       series catalog, deletions
  |     |--> Radarr                       movie catalog, deletions
  |     '--> Plex + Tautulli   or   Jellyfin      (one media server in use)
  '-- internal/store --> SQLite           DATA_DIR/hangarr.db
                         image cache      DATA_DIR/cache/img
```

Health checks and the calendar query the services live; their results are not stored. Run a single replica: the database is a SQLite file on a local volume.

## Stack

- **Go**, standard library first: `net/http` with method patterns (`GET /x/{id}`), `html/template` with `embed`, `log/slog`. There is no web framework.
- **HTMX 2** is vendored in `internal/web/static/`, with one hand-written script (`app.js`). There is no JavaScript build step and no CDN: the Content Security Policy allows scripts from `'self'` only.
- **CSS** is hand-written (`app.css`). The optional style sheets (`catppuccin.css`, `lumen.css`, `y2k.css`) load only when selected.
- **Charts** are SVG generated in Go (`internal/web/charts.go`). Each chart has a `<title>` and a data table.
- **SQLite** through `modernc.org/sqlite`, a pure-Go driver. The binary is built with `CGO_ENABLED=0`.
- **Migrations** are SQL files embedded in the binary (`internal/store/migrations/`).

## Packages

- `cmd/hangarr`: entry point. Subcommands: `serve`, `sync --once`, `admin reset-password`, `master-key`, `backup`, `healthcheck`, `version`.
- `cmd/mockarr`: serves `internal/mockarr` as fake Sonarr, Radarr, Plex, Jellyfin and Tautulli.
- `cmd/capture-fixtures`: records anonymized responses of the real services into `testdata/captured/`.
- `internal/actions`: deletions through Sonarr, Radarr or the media server, with dry-run, protection, typed confirmation and the action journal.
- `internal/auth`: first-start admin account, password policy and hashing, sessions, lockout and rate limits.
- `internal/clients/httpx`: HTTP layer shared by the API clients: timeouts, retries, bounded bodies, errors without credentials.
- `internal/clients/sonarr`, `internal/clients/radarr`: clients for the v3 APIs. Structs ignore unknown fields. They also carry the write calls used by the actions.
- `internal/clients/plex`: Plex Media Server client. Per-library watch counters are deliberately not decoded.
- `internal/clients/jellyfin`: Jellyfin API client.
- `internal/clients/tautulli`: Tautulli API v2 client, the watch-history source for Plex users.
- `internal/clients` (tests only): checks that the API structs decode the synthetic and captured responses.
- `internal/config`: loads and validates environment variables, including the `*_FILE` variants of secrets.
- `internal/i18n`: translation (English and French), number, size and date formatting.
- `internal/images`: proxy for posters and fanart (Sonarr, Radarr, Plex, Jellyfin), disk cache and resizing.
- `internal/matching`: pairs server items with Sonarr and Radarr items, and resolves history rows by title.
- `internal/mockarr`: fake upstream services built from `testdata/`.
- `internal/release`: quality hints (such as Remux) read from a video file name.
- `internal/scoring`: `watch_state` computation. Pure functions; the current time is injected.
- `internal/secretbox`: AES-256-GCM encryption of stored credentials.
- `internal/store`: the SQLite database: connection, migrations and every SQL query.
- `internal/syncer`: synchronization (fetch, store, match, link, merge, recompute), schedule, history-only runs, version and duplicate detection.
- `internal/upstream`: effective configuration of the services (environment or database), clients rebuilt atomically when it changes, service health checks.
- `internal/web`: HTTP server: routing, middleware (sessions, CSRF, CSP), handlers, templates, static assets, charts, Tautulli webhook.

## Sync pipeline

A run starts only when `Clients.Ready()` holds: every required service is configured and Plex and Jellyfin are not both configured. A run is started by the schedule (`syncer.Loop`), from the interface, or by `hangarr sync --once`. A Tautulli webhook starts a history-only run, debounced for 30 seconds. Only one run executes at a time.

1. **Fetch, in parallel.** Sonarr series, Radarr movies, the catalog of the active media server, and the watch histories (Tautulli, Plex history, or the played and in-progress items of each Jellyfin user). A failing source does not cancel the others.
2. **Store the catalogs.** The Sonarr and Radarr catalogs are replaced (`ReplaceArrCatalog`). Server version files are cached in `media_versions` (`ReplaceVersions`).
3. **Match.** `internal/matching` pairs each server item with a Sonarr or Radarr item, in this order:
   - exact TVDB, TMDB or IMDb id, tried for all items first, so a weaker match cannot take an item that has an id match;
   - the same file or folder on disk, compared by name so that different mount points do not matter; a name shared by several items is ignored;
   - normalized title (or original or alternate title) and year, within one year.

   The result is a `match_state` on each media row: `matched`, `arr_only` (Sonarr or Radarr only), `server_only` (media server only), or `hidden` (known only by the server that is not in use; frozen and not actionable). Each sync rewrites only the columns of the server it reads.
4. **Link history.** Each history event gets its media by Plex rating key first, then by normalized title and year for events whose key no longer exists (`matching.TitleIndex`).
5. **Merge histories.**
   - A Plex history entry that duplicates a Tautulli session (same viewer, item and date) is removed. The Tautulli session is kept, since it has durations and devices (`store.MergePlexHistory`).
   - A Jellyfin event is removed when it repeats a Plex or Tautulli play: same viewer, item and episode, and a stop time from 30 minutes before the Plex play started to 24 hours after it stopped (`store.MergeServerHistories`).
   - Jellyfin "played" marks made in bulk (20 or more items by the same viewer within the same minute, typically an import from Plex) match a Plex or Tautulli play of the same viewer and item, whatever the time.
   - A partial Plex play is marked as watched when Jellyfin says the item was watched, unless a finished Plex or Tautulli play already explains that Jellyfin event.
6. **Recompute.** Play counts, watch time, distinct viewers, last viewed date, season progress and `watch_state` (`never`, `in_progress`, `completed` or `abandoned`, from `scoring.WatchState`) are recomputed. Season 0 (specials) is excluded from progress and from `watch_state`. A series whose last season with files is finished is `completed`.

Viewers are Tautulli users, or Jellyfin users. Plex accounts unknown to Tautulli are dropped together with their events. With `HISTORY_SOURCE` set to `tautulli` or `plex`, the other history source is purged.

## Data model and migrations

Tables, by the migration that creates them:

- `app_users`, `sessions`, `auth_events`, `app_secrets` (`0001_auth.sql`): accounts, hashed session tokens, authentication journal, encrypted service secrets.
- `media` (`0002_data.sql`, then columns added by 0003, 0005, 0006 and 0007): one row per Sonarr series or Radarr movie, and per item known only to the media server. It holds the catalog fields, the server keys (`plex_rating_key`, `jellyfin_id`), `match_state`, the denormalized watch statistics and `watch_state`.
- `seasons` (`0002`): one row per season of a series, with episode counts, size and last viewed date.
- `watch_events` (`0002`): one row per history entry, with `source` set to `tautulli`, `plex_history` or `jellyfin_history`. `UNIQUE(source, source_id)` prevents the same history entry from being stored twice.
- `plex_accounts`, `sync_cursors`, `sync_runs` (`0002`): Plex accounts used to attach history to viewers, the sync position of each history source, and the history of synchronization runs.
- `app_settings` (`0004`): settings edited in the interface, such as the sync schedule and the service configuration (`svc.*` keys, secrets encrypted).
- `action_log` (`0005`): every deletion or protection attempt, dry-run included.
- `media_versions` and `duplicate_acks` (`0008`): file versions of movies, cached per server, and duplicate groups acknowledged for an exact set of versions.

`schema_migrations` is created by the store itself; it records the applied versions.

Migrations are rules, not suggestions:

- Files are named `NNNN_name.sql` and applied in version order at startup, each in a transaction.
- The schema only moves forward. A migration that has shipped is never edited, comments included: a change is a new file.
- Some comments in early migrations predate later changes (for example, the list of match states or the history source names). The code is the reference.
- Early migrations cite section numbers of the original specification, which is no longer part of the tree and remains in the git history. Where that specification differs from the code, the code wins.

## Actions and safety

- **Dry-run** is on by default (`DRY_RUN=true`). The server checks it for every action; the interface cannot bypass it. Dry-run attempts are journaled too.
- **Protection tags** (`PROTECTED_TAGS`, default `keep`): an item carrying one of them is refused. The tags are read live from Sonarr or Radarr just before the action, not taken from the database.
- **Local protection** (`protected_local`): an item deleted through the media server can be protected in Hangarr only. Nothing is sent to the media server for it.
- **Typed confirmation**: deleting a whole series requires its title when `CONFIRM_BY_TYPING` is on (default).
- **Season deletion** deletes the episode files first, then sends the series back to Sonarr with the season unmonitored (`sonarr.UnmonitorSeason`). Sonarr therefore does not download them again.
- **Batches** run sequentially, up to 100 items, with one media-server rescan per kind (series, movies) at the end.
- **Protection toggle** adds or removes the first tag of `PROTECTED_TAGS` by sending the raw object back (`PUT`), and leaves other tags unchanged.
- **Media server deletions**: an item known only by the active media server (no longer in Sonarr or Radarr) is deleted through the server (`DELETE` on Plex or Jellyfin), which removes the files. Items of the inactive server, and every server action while Plex and Jellyfin are both configured, are refused.
- **Journal**: every attempt is written to `action_log` and logged.

## Security model

- **Admin account**: created at the first start. Its generated password is printed once on stdout and must be changed at the first sign-in. The operator can instead set `HANGARR_ADMIN_PASSWORD`: the password must meet the policy, is not printed and is not forced to change. It is used only when the account does not exist yet.
- **Passwords** are hashed with argon2id (PHC string). The policy rejects passwords found in a common-password list.
- **Sessions** use random tokens. Only their SHA-256 hash is stored. The cookie is `HttpOnly` and `SameSite=Lax`; `Secure` follows `COOKIE_SECURE`.
- **Brute force**: from the 5th consecutive failure of a client (an IPv4 address, or an IPv6 /64 network), that client is locked out of the account for 1, 5, 15, then 60 minutes (60 minutes for every further failure). The other clients are not: someone who sends wrong passwords cannot lock the administrator out. Sign-in is also rate-limited per client (`LOGIN_MAX_PER_IP`, IPv6 counted per /64), a warning is logged every 100 failures on an account, and the Account page shows the failed sign-ins since the last successful one.
- **Client address**: `X-Forwarded-For` is trusted only from the proxies listed in `TRUSTED_PROXIES`.
- **CSRF**: every unsafe method on an authenticated route goes through `authed()`, which checks the CSRF token (header `X-CSRF-Token`, sent by HTMX from the layout, or form field `csrf_token`).
- **Content Security Policy**: `default-src 'self'`, `script-src 'self'`, `style-src 'self'`, `object-src 'none'`, `base-uri 'none'`, `frame-ancestors 'none'`, `form-action 'self'`. Templates contain no inline script, no inline style and no event-handler attribute.
- **Secrets** (service keys and tokens) are encrypted with AES-256-GCM, with a random nonce per value (`internal/secretbox`). The master key comes from `HANGARR_MASTER_KEY` or `HANGARR_MASTER_KEY_FILE`, which is the recommended setup because the key then stays outside the data volume. Without them, a key is generated at first start in `DATA_DIR/master.key` (mode 0600), and the interface warns about it.
- **Outbound requests** go through `internal/clients/httpx` only. Its redirect policy follows redirects to the same host only, so a credential never leaves for another host. Remote images are fetched over https only.
- **Tautulli webhook** (`POST /webhooks/tautulli`) is the only route without a session. It returns 404 unless its secret is configured, compares the `X-Hangarr-Token` header in constant time, and is rate-limited per IP. Its body is ignored.
- **Runtime**: the image is distroless and runs as a non-root user (65532). The root filesystem is read-only in `compose.yml` and in the Kubernetes manifest; only `/data` is writable. `/healthz` (liveness) and `/readyz` (readiness, which does not depend on Sonarr or Radarr) serve the probes.

## Plex or Jellyfin

Hangarr uses one media server at a time: Plex (with Tautulli for history) or Jellyfin (history read from Jellyfin itself).

- Setting a Plex variable (`PLEX_URL`, `PLEX_TOKEN`) together with a Jellyfin variable (`JELLYFIN_URL`, `JELLYFIN_API_KEY`) makes the application refuse to start.
- If both end up configured (for example, one set in the environment and the other in the interface), the state is `ServerConflict`: nothing is synchronized, server deletions are refused, and a banner is shown.
- Switching server deletes nothing. Items known only by the other server become `hidden`; their local protection and history are kept.
- The Tautulli card is disabled when Jellyfin is the server.
- Plex watch counters are not used: they describe the account of the token owner only. Watch statistics come from Tautulli, or from Plex's all-accounts history.
- The Jellyfin client authenticates with the `Authorization: MediaBrowser Token="..."` header.

## Internationalization

- English is the default (`DEFAULT_LANG=en`). The French catalog is `internal/i18n/fr.go`.
- Message keys are the English text. A key missing from the French catalog falls back to English. A `|context` suffix tells apart identical English texts; it is never displayed.
- Templates use `{{t "..."}}` and `{{tn n "one" "many"}}`; Go code uses `l.T(...)` and `l.N(...)`.
- The language comes from the `hangarr_lang` cookie (switched by `GET /lang/{code}`), otherwise from `DEFAULT_LANG`.
- `TestFrenchCatalogComplete` (`internal/web/i18n_test.go`) fails when a key has no translation.
- Numbers, sizes, dates and relative durations are formatted through the localizer.

## Compatibility with Cleanarr

The application was called Cleanarr before becoming Hangarr. Three fallbacks keep existing installations working. They are covered by tests; removing them is a breaking change for upgrades.

- `CLEANARR_MASTER_KEY` and `CLEANARR_MASTER_KEY_FILE` are read when the `HANGARR_` variant is absent, with a deprecation warning.
- The database `cleanarr.db` is renamed to `hangarr.db` at startup (`store.renameLegacyDB`). The rename never overwrites an existing `hangarr.db`.
- The Tautulli webhook also accepts the former `X-Cleanarr-Token` header.

## Repository layout

- `cmd/`: the binaries (see Packages).
- `internal/`: the application packages.
- `deploy/`: the Kubernetes manifest, the deployment check, and the Jellyfin lab used for real-server tests.
- `e2e/`: Playwright scenarios run against the runtime image.
- `testdata/`: the fixture scenario served by the mock (see `testdata/README.md`).
- `docs/`: documentation.
- `build/`: BuildKit configuration for multi-architecture images.
