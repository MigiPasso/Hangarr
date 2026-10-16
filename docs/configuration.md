# Configuration

Hangarr is configured with environment variables and, for the upstream services, on the **Settings › Services** page.
Every variable is optional: without any of them, Hangarr starts and waits for its services to be configured.
Configure the services in the interface when you can. A service variable set in the environment takes precedence and
locks its field in the interface.

General rules:

- An empty variable counts as unset.
- An invalid value stops the application at startup. All the errors are reported together.
- Durations use the Go syntax (`30m`, `12h`, `168h`). The `d` unit is not accepted.
- Booleans are `true` or `false`.
- A secret can be read from a file: set `NAME_FILE` to the path instead of `NAME`, for example
  `SONARR_API_KEY_FILE=/run/secrets/sonarr-api-key` with a Docker or Kubernetes secret. The trailing newline is removed.
  Setting both `NAME` and `NAME_FILE` stops the application at startup.

## Environment variables

### Server and storage

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Address the HTTP server listens on. The `healthcheck` command reads it too. |
| `DATA_DIR` | `/data` | Data directory: database `hangarr.db`, generated `master.key`, image cache (`cache/img`), backups (`backups/`). Must be writable. |

### Safety and cleanup

| Variable | Default | Description |
|---|---|---|
| `DRY_RUN` | `true` | Simulates deletions and protection changes: nothing is sent to Sonarr, Radarr or the media server, but each attempt is recorded in the action journal. Keep it `true` until you want deletions to happen. |
| `PROTECTED_TAGS` | `keep` | Comma-separated tags (case ignored). An item with one of them in Sonarr or Radarr is never proposed for cleanup nor deleted. The first tag is the one the **Protect** button adds; Hangarr creates it in Sonarr or Radarr if missing. If empty, protecting an item fails. |
| `CONFIRM_BY_TYPING` | `true` | Before a whole series is deleted, its title must be typed. In a batch that contains a series, the number of items must be typed. |
| `ABANDON_DAYS` | `90` | Days without a play after which a partly watched item is *abandoned* rather than *in progress*. Minimum `1`. |
| `PLEX_REFRESH_AFTER_DELETE` | `true` | After files are deleted (not in dry-run), asks the media server in use to rescan: the Plex libraries, or every Jellyfin library. |

### Sync and history

| Variable | Default | Description |
|---|---|---|
| `SYNC_TIME` | `04:00` | Start time of the syncs, `HH:MM` (24-hour clock, in `TZ`), until the frequency is saved in **Settings › General**: from then on, the syncs start from the time of the save. |
| `TZ` | `Europe/Paris` | Time zone of the sync schedule and of the dates shown in the interface, as an IANA name (for example `America/New_York`). The time zone data is built into the binary. |
| `HISTORY_SOURCE` | `auto` | Source of the watch history: `auto`, `merged`, `tautulli` or `plex` (case ignored). See [History sources](#history-sources). |

The sync frequency is set in **Settings › General**: every 24, 12, 8, 6, 4, 3, 2 or 1 hours (once a day by default).
The syncs start from the time of the save and repeat at the same times each day: saved at `04:00` with a frequency of
6 hours, they run at 04:00, 10:00, 16:00 and 22:00. When the application starts and the last sync is older than the interval (or none has
run yet), a sync runs immediately.

### Authentication and sessions

| Variable | Default | Description |
|---|---|---|
| `ADMIN_USERNAME` | `admin` | Name of the account created at the first start, 1 to 64 characters. Only used when that account is created. Also the default account of `hangarr admin reset-password`. |
| `HANGARR_ADMIN_PASSWORD` | empty | Password of the admin account, instead of a generated one. `HANGARR_ADMIN_PASSWORD_FILE` reads it from a file (a Docker or Kubernetes secret). See [Admin password](#admin-password). |
| `COOKIE_SECURE` | `true` | Sets the `Secure` attribute on the session cookies, so they are sent over HTTPS only. Use `false` only for plain-HTTP access on a local network. The sign-in page warns when it is served over HTTP while this is `true` (localhost excepted). With `true`, the cookies are named `__Host-hangarr_session` and `__Secure-hangarr_presession` (a subdomain cannot set them), and HTTPS responses carry `Strict-Transport-Security` (HTTPS seen directly, or `X-Forwarded-Proto: https` from a `TRUSTED_PROXIES` proxy). |
| `HSTS_INCLUDE_SUBDOMAINS` | `false` | Adds `includeSubDomains` to `Strict-Transport-Security`. Leave it off when Hangarr is served on a parent domain (such as `home.lan`): browsers would then refuse plain HTTP on every service below it for a year. |
| `ALLOWED_HOSTS` | empty | Host names (comma-separated) under which Hangarr may be reached over **plain HTTP**, besides the local ones always accepted: `localhost`, IP addresses, names without a dot (`hangarr`, `nas`) and names under `.local`, `.lan`, `.home.arpa`, `.internal`, `.localdomain`. Any other name gets `421 Misdirected Request`: this blocks DNS rebinding, where a web page makes the browser of someone on your network reach Hangarr under the attacker's domain. HTTPS requests (directly, or through a `TRUSTED_PROXIES` proxy) are not checked: the certificate already rules that out. `*` turns the check off. `/healthz`, `/readyz` and the webhook are never checked. |
| `SESSION_IDLE_TIMEOUT` | `12h` | A session expires after this much inactivity. Minimum `1m`. |
| `SESSION_MAX_AGE` | `168h` | Absolute lifetime of a session. Minimum `1m`, and not shorter than `SESSION_IDLE_TIMEOUT`. |
| `LOGIN_MAX_PER_IP` | `10` | Sign-in attempts allowed per client in a 15-minute sliding window, successful ones included: per IPv4 address, per /64 network in IPv6. The password change shares this limit. Minimum `1`. |
| `AUTH_EVENTS_RETENTION_DAYS` | `365` | Days the security log (sign-ins, failures, configuration changes) is kept. Repeated failures of a client within 15 minutes are stored as one row with a count. `0` keeps everything. |
| `ACTION_LOG_RETENTION_DAYS` | `0` | Days the actions log (deletions, protections) is kept. `0` (default) keeps everything. |
| `TRUSTED_PROXIES` | empty | Comma-separated IP addresses or CIDR ranges of the reverse proxies allowed to set `X-Forwarded-For`, for example `172.18.0.0/16`. Other requests are identified by their own address. |

### Interface and logging

| Variable | Default | Description |
|---|---|---|
| `DEFAULT_LANG` | `en` | Interface language for visitors who have not chosen one: `en` or `fr`. The EN / FR link in the top bar stores the choice in the `hangarr_lang` cookie. |
| `LOG_FORMAT` | `json` | `json` or `text`. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (case ignored). slog offsets such as `info+2` are accepted. |

### Services

Each service needs its address and its credential. Sonarr, Radarr and one media server are required for the sync. Tautulli
is required only when `HISTORY_SOURCE` is `merged` or `tautulli`. See [Services](#services) and [Plex or Jellyfin](#plex-or-jellyfin).

| Variable | Default | Description |
|---|---|---|
| `SONARR_URL` | empty | Sonarr API URL, for example `http://sonarr:8989`. |
| `SONARR_API_KEY` | empty | Sonarr API key. Accepts `_FILE`. |
| `SONARR_PUBLIC_URL` | empty (API URL) | Address the browser opens for the "open in Sonarr" links. |
| `RADARR_URL` | empty | Radarr API URL, for example `http://radarr:7878`. |
| `RADARR_API_KEY` | empty | Radarr API key. Accepts `_FILE`. |
| `RADARR_PUBLIC_URL` | empty (API URL) | Address the browser opens for the "open in Radarr" links. |
| `PLEX_URL` | empty | Plex server URL, for example `http://plex:32400`. |
| `PLEX_TOKEN` | empty | Plex token (`X-Plex-Token`). Accepts `_FILE`. |
| `PLEX_PUBLIC_URL` | empty (app.plex.tv) | Base address of Plex Web, for example `https://plex.example.com:32400`. The "open in Plex" links go to `<address>/web/index.html`. |
| `JELLYFIN_URL` | empty | Jellyfin server URL, for example `http://jellyfin:8096`. |
| `JELLYFIN_API_KEY` | empty | Jellyfin API key. Accepts `_FILE`. |
| `JELLYFIN_PUBLIC_URL` | empty (API URL) | Address the browser opens for the "open in Jellyfin" links. |
| `TAUTULLI_URL` | empty | Tautulli API URL, for example `http://tautulli:8181`. |
| `TAUTULLI_API_KEY` | empty | Tautulli API key. Accepts `_FILE`. |
| `TAUTULLI_WEBHOOK_SECRET` | empty | Secret of the [Tautulli webhook](#tautulli-webhook): at least 32 characters. Empty disables the webhook. Accepts `_FILE`. |

Public URLs are needed when the API URL is not reachable from the browser, for example a cluster-internal service name.

### Master key

| Variable | Default | Description |
|---|---|---|
| `HANGARR_MASTER_KEY` | empty | Key that encrypts the saved credentials: 32 random bytes, as 64 hexadecimal characters or as base64. Accepts `_FILE`. Empty: a key is generated in `DATA_DIR/master.key`. See [Master key](#master-key). |
| `CLEANARR_MASTER_KEY` | empty | Former name of `HANGARR_MASTER_KEY`, still read when that one is not set. Accepts `_FILE`. See [Deprecated names](#deprecated-names). |

## Master key

The credentials entered in the interface (API keys, tokens, webhook secret) are encrypted with AES-256-GCM before being
stored in the database. A copy of the database alone does not reveal them: the master key is also needed.

### Recommended: provide the key

1. Generate a key with the image (for example `ghcr.io/migipasso/hangarr:v1.3.1`). The command prints it and writes nothing to disk:

   ```sh
   docker run --rm <image> master-key
   ```

   Any base64 string or 64-character hexadecimal string that decodes to 32 bytes is accepted, so `openssl rand -base64 32`
   also works.
2. Store the key in a secret and expose it as `HANGARR_MASTER_KEY_FILE` (or `HANGARR_MASTER_KEY`). Setting both is refused.
3. Keep the key outside the data volume. A backup of the database is only useful with the key: keep both.

### Default: generated key

Without a provided key, Hangarr generates one the first time it is needed, stores it in `DATA_DIR/master.key` (by default
`/data/master.key`, mode `0600`) and logs a warning. Whoever gets a copy of the volume then gets the credentials too, and
the Services page shows a warning to that effect. If no key is provided and the key file cannot be created (for example,
`DATA_DIR` is read-only at the first start), the application does not start. On file systems without hard links (SMB/CIFS,
some NFS or FUSE mounts), the file is created directly instead; two instances starting at the very same moment on such a
volume may then fail once, and the next start works.

### Switching to a provided key

Set `HANGARR_MASTER_KEY` (or `HANGARR_MASTER_KEY_FILE`) and restart. If `master.key` exists, Hangarr re-encrypts the saved
credentials with the provided key, then deletes `master.key`. Nothing has to be entered again. If the re-encryption fails,
the file is kept and the attempt is repeated at the next start.

### Changing or removing the provided key

Credentials encrypted with one key cannot be decrypted with another. After a change, the affected fields show
"Saved value unreadable (master key changed?): enter it again." Removing `HANGARR_MASTER_KEY` has the same effect: the
application then generates a new `master.key` in `DATA_DIR`, which does not open the earlier credentials. No command
rotates the key: to change it, enter the credentials again.

## Services

The **Settings › Services** page has one card per service:

- Sonarr and Radarr: API URL, Public URL, API key.
- Plex: Server URL, Public URL, Token (`X-Plex-Token`).
- Jellyfin: Server URL, Public URL, API key.
- Tautulli: API URL, API key, Webhook secret.

Saving, testing a connection or generating the webhook secret needs your password from less than 10 minutes ago: right
after signing in nothing is asked, later the page asks you to confirm it first. A connection test sends a request to any
address you type, with the saved keys: this keeps a stolen session from using the page against your network.

Where to find the credentials:

- Sonarr and Radarr: Settings → General → Security → API Key.
- Plex: in Plex Web, ⋯ → Get Info → View XML; the URL contains `X-Plex-Token=…`. Use the token of the server owner.
- Jellyfin: Dashboard → API Keys → +.
- Tautulli: Settings → Web Interface → API; tick "Enable API".

Rules:

- **Locked fields.** A field whose variable is set in the environment shows "Set by the environment" and cannot be edited.
- **Secrets.** Credentials are never sent back to the browser. A saved one is shown as `••••` followed by its last four
  characters (only `••••` for values shorter than 12 characters). An empty field keeps the saved value. "Clear the saved
  value" removes it.
- **Addresses.** An address must be an `http://` or `https://` URL with a host, without user name or password. Trailing
  slashes are removed. Emptying an address field removes the saved address.
- **Public URLs** are the address of the application itself, not one of its pages. Addresses with a query or a fragment,
  or pointing to a page of the application (`/login`, `/movie/…`, `/settings/…`, or `/web` for Plex and Jellyfin), are
  refused. A base path such as `https://host/radarr` is accepted.
- **Changing an address.** If the API address of a service changes its scheme, host or port, its saved credential must be
  typed again: saving or testing is refused until then. This keeps a credential from being sent to another server. If the
  credential is set by the environment, change the address in the environment too. The webhook secret is not affected.
- **Changes** are recorded in **Log › Security** (service, user, time). The log lines name the fields changed, never
  their values.
- **Test connection** checks the address and the credential. An address that answers as another application is refused
  (for example a Radarr URL in the Sonarr card), and so is Emby in the Jellyfin card.
- **First sync.** When a save makes the configuration complete, the first sync starts. Without the required services, the
  application still runs: the sync is disabled and a banner lists the missing services.

## Plex or Jellyfin

Hangarr reads one media server at a time: Plex or Jellyfin.

- Configure one of them, not both: `PLEX_URL` with `PLEX_TOKEN`, or `JELLYFIN_URL` with `JELLYFIN_API_KEY`. Setting a
  Plex variable (`PLEX_URL` or `PLEX_TOKEN`) together with a Jellyfin variable (`JELLYFIN_URL` or `JELLYFIN_API_KEY`) stops
  the application at startup.
- In the interface, the card of the other server is greyed out while one is configured, and saving it is refused. Clearing
  a configuration is always allowed. Tautulli's card is greyed out while Jellyfin is used.
- If both are configured anyway (one in the environment, one in the interface), no synchronization runs and deletions
  through the media server are refused until one of them is cleared. A banner says so.
- To switch servers, clear the configuration of the current one, then configure the other. Nothing is purged: the items
  that only the other server knows are hidden (not deleted, no action possible), and keep their local protections and
  their watch history. They come back when that server is used again.
- A play recorded by both servers is counted once: a Jellyfin play that repeats a Plex or Tautulli play of the same item and
  viewer (from 30 minutes before to 24 hours after it) is dropped, and the Plex or Tautulli play is kept.
- Emby is not supported. Tautulli is only used with Plex: with Jellyfin, `HISTORY_SOURCE` is ignored.

## History sources

`HISTORY_SOURCE` applies to Plex. With Jellyfin, the Jellyfin history is used and the variable is ignored.

| Value | Source |
|---|---|
| `auto` (default) | `merged` when Tautulli is configured, `plex` otherwise. |
| `merged` | Plex history (what was watched), enriched by Tautulli: play durations, devices and partial plays. Requires Tautulli. A play known only from Plex gets an estimated duration, the runtime from Sonarr or Radarr. |
| `tautulli` | Tautulli only. Requires Tautulli. |
| `plex` | Plex history only. It lists the plays of the items still in the Plex library, without durations. Tautulli is not needed. |

The Jellyfin history keeps the last play of each item for each user, without durations. Plays in progress are listed with
their progress.

## Tautulli webhook

Without a webhook, the history is read by the scheduled sync. With one, Tautulli tells Hangarr when a play stops, and
Hangarr re-reads the history about 30 seconds later. The notifications received during that delay are handled by the same
read. The webhook only refreshes the history, not the library.

Setup:

1. Generate the secret in **Settings › Services**, in the Tautulli card, with **Generate a secret**. It is shown once. You
   can also set `TAUTULLI_WEBHOOK_SECRET` (at least 32 characters). Generating a new secret replaces the old one: update
   Tautulli.
2. In Tautulli, go to Settings → Notification Agents, and add a Webhook:
   - Webhook URL: `http://<hangarr-address>:8080/webhooks/tautulli`, the address Tautulli uses to reach Hangarr. The
     Services page shows the exact URL.
   - Method: `POST`.
   - Triggers: *Playback Stop* and *Watched*.
   - Data tab, *JSON Headers*, for both triggers: `{"X-Hangarr-Token": "<secret>"}`.

Behaviour of `POST /webhooks/tautulli`:

- No session is required. The body is ignored: Hangarr reads the history back from the Tautulli API.
- The token is read from the `X-Hangarr-Token` header and compared in constant time. A wrong or missing token gets `401`.
- Without a secret, or while Jellyfin is the media server, the route answers `404`.
- More than 60 requests per minute from one IP address get `429`, with a `Retry-After` header.
- Accepted requests get `202` with `{"status":"accepted"}`.

The Tautulli-side steps are also in the [deployment guide, section 6](deployment.md#6-tautulli-webhook).

## Admin password

- **Without `HANGARR_ADMIN_PASSWORD`**, the first start generates a password and prints it once, on standard output (it
  shows in `docker logs` or `kubectl logs`). It must be changed at the first sign-in.
- **With `HANGARR_ADMIN_PASSWORD`**, the password is the one you set. It is neither printed nor required to change at the
  first sign-in. Remove the variable once the account exists: it is then ignored and a warning is logged.
- **Policy.** 12 to 128 characters, with at least one lowercase letter, one uppercase letter, one digit and one symbol
  (any character that is not a letter or a digit). It must not contain the admin username and must not be a common
  password. The policy is checked at every start while the variable is set, even if the account already exists: a value
  that does not meet it stops the application at startup.
- The value is used as typed, spaces included. From `HANGARR_ADMIN_PASSWORD_FILE`, only the line break that ends the
  file is dropped.
- The variable is only used to create the account. Prefer the `_FILE` variant: an environment variable stays visible
  in `docker inspect`, `/proc/1/environ` and `kubectl describe` for as long as it is set. On Kubernetes, delete the
  Secret (or remove it from the Deployment) once the account exists.
- **Lost password.** `hangarr admin reset-password [--username NAME]` prints a new temporary password once, requires a
  change at the next sign-in, and revokes every session of the account. With Docker Compose:
  `docker compose exec hangarr /hangarr admin reset-password`.

## Deprecated names

Hangarr was called Cleanarr. These names still work:

- `CLEANARR_MASTER_KEY` and `CLEANARR_MASTER_KEY_FILE` are read when `HANGARR_MASTER_KEY` and `HANGARR_MASTER_KEY_FILE`
  are both unset. When the Hangarr variable is set, the Cleanarr one is ignored. A warning is logged at startup.
- The `X-Cleanarr-Token` header is accepted by the Tautulli webhook in place of `X-Hangarr-Token`. A warning is logged
  for each request that uses it.
- A `cleanarr.db` file in `DATA_DIR` is renamed to `hangarr.db` at startup, when `hangarr.db` does not exist.
