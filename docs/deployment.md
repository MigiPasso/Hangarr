# Deploying Hangarr

This guide covers deploying with **Docker** or **Kubernetes**,
connecting Sonarr, Radarr, Plex or Jellyfin and Tautulli, checking the installation, day-to-day operations and common
problems. The full list of environment variables is in [configuration.md](configuration.md); the project overview is
in the [README](../README.md).

**Contents**

- [1. Before you start](#1-before-you-start)
- [2. Docker](#2-docker)
- [3. Kubernetes](#3-kubernetes)
- [4. First sign-in](#4-first-sign-in)
- [5. Connecting the services](#5-connecting-the-services)
- [6. Tautulli webhook](#6-tautulli-webhook)
- [7. Checking the installation](#7-checking-the-installation)
- [8. Operations](#8-operations)
- [9. Troubleshooting](#9-troubleshooting)

---

## 1. Before you start

**You need**

- Sonarr, Radarr, and Plex or Jellyfin, reachable from the place where Hangarr runs. Tautulli is optional (see [5](#5-connecting-the-services)).
- The access keys of these services.
- Either Docker with Compose v2 (2.24 or later), or a Kubernetes cluster. Docker Engine 24 or later is recommended.

**Where to run it**

| Situation | Advice |
|---|---|
| The *arr stack runs in a Kubernetes cluster | Deploy Hangarr in the **same namespace**: services are reached by their short names (`http://sonarr:8989`) and traffic stays inside the cluster. |
| The *arr stack runs in Docker on a server | Run `compose.yml` on the same machine, on the same Docker network (see [2](#2-docker)). |
| Testing from a workstation | Run `compose.yml` on the workstation and reach the services at their addresses, for example over Tailscale. |

**Safe by default:** `DRY_RUN=true`. Deletions are simulated and logged, and the interface shows a banner saying so.
Set `DRY_RUN=false` only once the cleanup proposals have been checked. Set it in `.env` with Docker, or in the
`hangarr-config` ConfigMap with Kubernetes.

---

## 2. Docker

```sh
cp .env.example .env                 # optional: DRY_RUN, COOKIE_SECURE, TZ, DEFAULT_LANG…
docker compose up -d                 # pulls ghcr.io/migipasso/hangarr:v1.3.1; another release: set HANGARR_IMAGE in .env
docker compose logs hangarr | grep -A4 "INITIAL ADMIN"
```

Open `http://<host>:8080` (or `http://localhost:8080`). The published port is `HANGARR_PORT` (default `8080`) in
`.env`. The initial password is explained in [4](#4-first-sign-in).

- **Plain HTTP on the local network**: set `COOKIE_SECURE=false` in `.env`. Otherwise the browser refuses the session
  cookie, and the sign-in page says so. On `localhost` this is not needed.
- **Behind an HTTPS reverse proxy**: keep `COOKIE_SECURE=true` and set the address of the proxy in `TRUSTED_PROXIES`.
- **Same network as the *arr stack**: in `compose.yml`, uncomment `networks: [default, arr]` in the `hangarr` service,
  and the top-level `networks` block at the end of the file. Set `name:` to your stack's network (`docker network ls`).
  Sonarr is then reachable at `http://sonarr:8989`, and Tautulli can call `http://hangarr:8080/webhooks/tautulli`.
- **Master key (recommended)**: generate it once, outside the data volume:
  ```sh
  docker run --rm ghcr.io/migipasso/hangarr:v1.3.1 master-key > master.key
  ```
  Then, in `compose.yml`, uncomment `secrets: [hangarr_master_key]` and the `environment` block
  (`HANGARR_MASTER_KEY_FILE`) of the `hangarr` service, and the top-level `secrets` block. Keep `master.key` out of
  version control. Without a master key, one is generated in the data volume (see [8](#8-operations)).
- **Without Compose**:
  ```sh
  docker run -d --name hangarr --restart unless-stopped -p 8080:8080 --env-file .env \
    -v hangarr-data:/data --read-only --user 65532:65532 --cap-drop ALL \
    --security-opt no-new-privileges:true ghcr.io/migipasso/hangarr:v1.3.1
  ```
  `--env-file` requires the file to exist (it can be empty). To keep the data in a host folder, use
  `-v /srv/hangarr:/data` and first run `sudo chown -R 65532:65532 /srv/hangarr`.
- The container runs as user 65532, with a read-only root filesystem, no Linux capabilities and
  `no-new-privileges`. Only `/data` is writable. The image has a built-in health check (`hangarr healthcheck`).

From the repository, `make deploy-check` validates `compose.yml` and runs it with an image built from the current sources, against fake services.

---

## 3. Kubernetes

[`deploy/kubernetes/hangarr.yaml`](../deploy/kubernetes/hangarr.yaml) is a plain YAML manifest, without kustomize. It
contains a ConfigMap (`hangarr-config`), a PersistentVolumeClaim (`hangarr-data`, 5Gi; 10Gi for a large library), a
Deployment and a Service (port 8080). It sets **no namespace**: use `-n <ns>`, or the namespace of your Argo CD
application.

The Deployment runs **one replica** (SQLite on a single volume): keep `replicas: 1`. It uses the `Recreate` strategy,
so the old pod stops before the new one starts. It runs as non-root with a read-only root filesystem, and has startup,
liveness (`/healthz`) and readiness (`/readyz`) probes.

### 3.1 Set the image

The manifest uses the public image `ghcr.io/migipasso/hangarr:v1.3.1`, which needs no pull secret. To deploy another
release, change the tag:

```yaml
image: ghcr.io/migipasso/hangarr:v1.3.1
```

### 3.2 Create the master key secret

In the same namespace as the Deployment, and never in Git:

```sh
kubectl -n <ns> create secret generic hangarr-master-key \
  --from-literal=master-key="$(openssl rand -base64 32)"
```

Keep a copy of this key in a password manager. It encrypts the API keys entered in the interface. If it is lost or
changed, those keys must be entered again.

### 3.3 Apply

```sh
kubectl -n <ns> apply -f deploy/kubernetes/hangarr.yaml
kubectl -n <ns> get pods -l app.kubernetes.io/name=hangarr -w
```

With Argo CD, point the application at this file and choose the destination namespace. The secret is created by hand
and is not managed by Argo CD.

### 3.4 Access the application

The simplest way is a port-forward:

```sh
kubectl -n <ns> port-forward svc/hangarr 8080:8080
```

Then open `http://localhost:8080`. Keep `COOKIE_SECURE: "true"` in the ConfigMap: browsers accept Secure cookies on
`localhost`.

For access through an HTTPS Ingress, keep `COOKIE_SECURE: "true"` and set `TRUSTED_PROXIES` to the CIDR of the ingress
controller pods. Example with the Tailscale Kubernetes operator (an example, not a requirement; adapt it to your
ingress controller):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hangarr
spec:
  ingressClassName: tailscale
  defaultBackend:
    service:
      name: hangarr
      port:
        number: 8080
  tls:
    - hosts:
        - hangarr        # https://hangarr.<tailnet>.ts.net
```

---

## 4. First sign-in

On the first start with an empty database, Hangarr creates the `admin` account and writes its password to the logs,
**once**:

```sh
kubectl -n <ns> logs deploy/hangarr | grep -A4 "INITIAL ADMIN"   # Kubernetes
docker compose logs hangarr | grep -A4 "INITIAL ADMIN"          # Docker
```

If the banner is not in the logs (the pod was recreated since, or the account already existed), set a new temporary
password:

```sh
kubectl -n <ns> exec deploy/hangarr -- /hangarr admin reset-password
docker compose exec hangarr /hangarr admin reset-password
```

Signing in forces a new password. The interface is in English. The **FR** link in the top bar switches it to French,
and the browser keeps the choice. `DEFAULT_LANG=fr` makes French the default for the instance.

**Choosing the password in advance.** Set `HANGARR_ADMIN_PASSWORD` before the first start: in `.env` with Docker, or
on Kubernetes in the `env` of the Deployment, read from a Secret:

```yaml
env:
  - name: HANGARR_ADMIN_PASSWORD
    valueFrom:
      secretKeyRef:
        name: <secret>
        key: <key>
```

The account is then created with this password: nothing is written to the logs, and no change is required. The
password must meet the same rules as a chosen one: at least 12 characters, with lowercase and uppercase letters, a
digit and a symbol, not containing the username, and not a common password. Otherwise Hangarr refuses to start, even if
the account already exists. `HANGARR_ADMIN_PASSWORD_FILE` reads it from a file instead, such as a mounted Docker or
Kubernetes secret, which keeps it out of the container configuration. It is only read when the account is created;
afterwards it is ignored, with a warning in the logs. Remove it once the account exists: otherwise it stays readable in
the container configuration (`docker inspect`, `kubectl describe`); on Kubernetes, delete the Secret too.

---

## 5. Connecting the services

Open **Settings › Services**. For each service, enter the address and the key, click **Test connection**, then
**Save**. The first sync starts by itself as soon as the configuration is complete: Sonarr, Radarr and one media server
(Plex or Jellyfin, not both), plus Tautulli when `HISTORY_SOURCE` requires it.

The **API URL** is the base address of the service: the address of its web interface, without `/api` (Hangarr adds
it). Include the URL base if the service has one (for example `https://host/sonarr`), and the port if it is not the
default one for the scheme.

| Service | Address (same Kubernetes namespace) | Key |
|---|---|---|
| Sonarr | `http://sonarr:8989` | *Settings → General → Security → API Key* |
| Radarr | `http://radarr:7878` | *Settings → General → Security → API Key* |
| Plex | `http://plex:32400` | `X-Plex-Token` (see below) |
| Jellyfin | `http://jellyfin:8096` | *Dashboard → API Keys* (see below) |
| Tautulli | `http://tautulli:8181` | *Settings → Web Interface → API*, tick *Enable API* |

Find the names and ports of your Services with `kubectl -n <ns> get svc`. Outside the cluster, use the address you open
in a browser, for example `https://sonarr.<tailnet>.ts.net`.

> Enter the **final address**. If an address redirects to another host, Hangarr refuses it so that the key is not sent
> there, and the connection test explains why.

### Plex server address

Use the **Plex Media Server** address (port 32400), not `app.plex.tv`. To check it, without a token:

```sh
curl -s http://plex:32400/identity     # short XML with machineIdentifier and version
```

If Plex refuses plain HTTP (*Settings → Network → Secure connections: Required*) and Hangarr runs outside the cluster,
do one of the following:

- Use the server's HTTPS `plex.direct` address, found with a token:
  ```sh
  curl -s -H "Accept: application/json" \
    "https://plex.tv/api/v2/resources?includeHttps=1&X-Plex-Token=<token>" \
    | grep -o '"uri":"https://[^"]*plex.direct:32400"'
  ```
- Put an HTTPS Ingress in front of Plex, for example through Tailscale.
- Add the network Hangarr connects from to the LAN networks of Plex (`100.64.0.0/10` for Tailscale: the traffic stays
  inside the tailnet).

The Plex **Public URL** can stay empty: "Open in Plex" links then go through app.plex.tv.

### Plex token

Use the token of the **server owner's account**: a shared account only sees its own data. Either:

- in Plex Web, open a movie or an episode, then **⋯ → Get Info → View XML**: the URL ends with `X-Plex-Token=…`;
- or on the server, if Plex runs in your cluster as a `plex` Deployment (adapt the names and the path otherwise):
  ```sh
  kubectl -n <ns> exec deploy/plex -- grep -o 'PlexOnlineToken="[^"]*"' \
    "/config/Library/Application Support/Plex Media Server/Preferences.xml"
  ```

The token gives access to the whole Plex account: do not share it. Hangarr stores it encrypted and never displays it
again. If it changes (for example after you sign out of all devices), enter the new one.

### Jellyfin

Jellyfin replaces Plex as the media server: configure one or the other, never both (see
[configuration.md](configuration.md)). While Plex is configured, the Jellyfin card is greyed out: clear Plex first.

- **Server address**: for example `http://jellyfin:8096` in the same namespace.
- **API key**: *Dashboard → API Keys → +*. It is stored encrypted.
- **Public URL** (optional): the address the browser opens for "Open in Jellyfin" links. It defaults to the server
  address. An address ending in `/web` is refused.

The Jellyfin history comes from Jellyfin itself: Tautulli is not used. Hangarr only works with Jellyfin: the connection
test refuses other servers, such as Emby.

### Tautulli

Tautulli is only used with Plex: its card is greyed out with Jellyfin. With Plex, it is optional, and adds exact play
durations, devices and partial plays. `HISTORY_SOURCE` (see [configuration.md](configuration.md)) decides whether it is
required: the `tautulli` and `merged` values need it. After saving, click **Sync now** to read the history.

---

## 6. Tautulli webhook

With Plex, a webhook is recommended. Without it, the statistics follow the scheduled sync (time and frequency set in
**Settings**). With it, Tautulli tells Hangarr when a playback stops or is marked as watched, and the history is read
about 30 seconds later. The content of Tautulli's call is ignored: it only triggers reading the history through the API.

The route answers 404 without a webhook secret. It also answers 404 while Jellyfin is the media server, since Tautulli
is not read then: the scheduled sync, or **Sync now**, updates the history instead.

1. In the Tautulli card of **Settings › Services**, in the *Webhook* section, click **Generate a secret**. Copy it: it is
   shown only once. Alternatively, set `TAUTULLI_WEBHOOK_SECRET` (at least 32 characters).
2. In Tautulli, go to *Settings → Notification Agents → Add a new notification agent → Webhook*:
   - **Configuration**: *Webhook URL* `http://hangarr:8080/webhooks/tautulli`, in the same namespace or Docker network
     (on Kubernetes: `http://hangarr.<ns>.svc.cluster.local:8080/webhooks/tautulli`). If Tautulli does not share a
     network with Hangarr, use the address under which it reaches Hangarr. *Webhook Method*: `POST`.
   - **Triggers**: **Playback Stop** and **Watched**.
   - **Data**: for both triggers, in *JSON Headers*:
     ```json
     {"X-Hangarr-Token": "<secret>"}
     ```
   - **Save**.
3. To test, start and then stop a playback in Plex, and check the logs:
   ```sh
   kubectl -n <ns> logs deploy/hangarr | grep -i webhook    # Kubernetes
   docker compose logs hangarr | grep -i webhook            # Docker
   ```
   Expected: `tautulli webhook received`, then `history sync finished` about 30 seconds later.

---

## 7. Checking the installation

- **Health** (*System → Health*): the services shown as *Reachable*, with their version, response time and the alerts
  of Sonarr and Radarr; the synchronization history; and the data gaps between the media server and Sonarr/Radarr
  (items found on only one side, approximate matches).
- **Series**, **Movies** and **Stats**: your data.
- **Cleanup**: what nobody has watched for 180 days by default (adjustable in **Settings**). While `DRY_RUN=true`,
  deletions are simulated and listed in **Log**.
- **Duplicates**: movies that the media server has more than once (several versions, or several libraries). A version
  is deleted from its row, through the media server. On Plex, *Allow media deletion* must be enabled (*Settings ›
  Library*). On Jellyfin, deletion is refused when another version is in the same folder, because Jellyfin would delete
  the whole folder. While `DRY_RUN=true`, deletions are simulated.
- **Protect**: button on an item's page. It adds the first tag of `PROTECTED_TAGS` (`keep` by default) in Sonarr or
  Radarr. For an item known only to the media server, protection is kept in Hangarr only.

---

## 8. Operations

### Updates

- **Kubernetes**: change `image:` to the new release (`ghcr.io/migipasso/hangarr:vX.Y.Z`), then apply the manifest or
  sync Argo CD. Database migrations run at startup.
- **Docker**: set `HANGARR_IMAGE=ghcr.io/migipasso/hangarr:vX.Y.Z` in `.env` (or change the default tag in `compose.yml`),
  then run `docker compose pull && docker compose up -d`.
- **Your own build** instead of the published image: run `make image-load` first (it builds `hangarr:local` from the
  sources), then set `HANGARR_IMAGE=hangarr:local` in `.env` and run `docker compose up -d`.

### Backup

This writes a consistent copy of the database while Hangarr keeps running, to `/data/backups/hangarr-YYYYMMDD-HHMMSS.db`:

```sh
kubectl -n <ns> exec deploy/hangarr -- /hangarr backup
docker compose exec hangarr /hangarr backup
```

To choose the destination, pass a path: `/hangarr backup /data/backups/<file>.db`.

The API keys saved in the database are encrypted with the master key, which is not in the backup. Keep the master key
with the backup: the `hangarr-master-key` secret on Kubernetes, `HANGARR_MASTER_KEY` or `master.key` on Docker. Without
a master key from the environment, `master.key` sits in the data volume: a copy of the volume then exposes the secrets.

**Restore (Docker)**: stop Hangarr, copy the backup over `hangarr.db` in the volume, and start it again. `docker volume ls`
shows the exact volume name (`hangarr_hangarr-data` with the project name `hangarr`).

```sh
docker compose stop hangarr
docker run --rm --user 65532:65532 -v hangarr_hangarr-data:/data alpine sh -c \
  'rm -f /data/hangarr.db-wal /data/hangarr.db-shm && cp /data/backups/<file>.db /data/hangarr.db'
docker compose start hangarr
```

**Restore (Kubernetes)**: scale `deploy/hangarr` to 0, copy the backup over `hangarr.db` in the `hangarr-data` claim
with a temporary pod running as user 65532, then scale back to 1.

### Synchronization from the command line

`hangarr sync --once` runs one synchronization and prints a summary. `hangarr help` lists the other subcommands.

```sh
kubectl -n <ns> exec deploy/hangarr -- /hangarr sync --once
docker compose exec hangarr /hangarr sync --once
```

### Lost admin password

See [4](#4-first-sign-in): `admin reset-password` prints a new temporary password.

---

## 9. Troubleshooting

### `secret "hangarr-master-key" not found`

The secret does not exist **in the namespace of the pod**. Create it ([3.2](#32-create-the-master-key-secret)). The
kubelet retries on its own; there is no need to redeploy.

### `namespaces "hangarr" not found` (Argo CD)

A resource has a `namespace:` that does not exist. The manifest sets none. Remove any `namespace:` line from your copy,
and let Argo CD use the namespace of the application.

### 502 Bad Gateway from the Ingress

The proxy cannot reach the pod:

```sh
kubectl -n <ns> get pods -l app.kubernetes.io/name=hangarr
kubectl -n <ns> get endpoints hangarr        # must list IP:8080
```

- Empty endpoints: the `selector` of the Service does not match the pod labels (`app.kubernetes.io/name: hangarr`).
- Pod not ready: `kubectl -n <ns> describe pod -l app.kubernetes.io/name=hangarr` (events at the bottom), and
  `kubectl -n <ns> logs deploy/hangarr`.
- The Ingress backend points to port 80 instead of **8080**.

### Sign-in fails over HTTP, with no message

`COOKIE_SECURE=true` while the page is served over plain HTTP (outside `localhost`): the browser drops the session
cookie. Use HTTPS, or set `COOKIE_SECURE=false` for a local test. The sign-in page shows a warning in this case.

### "Access denied (HTTP 401): check the API key or token."

Wrong API key or token. For Plex, use the token of the server owner's account. For Jellyfin, use an API key created in
*Dashboard → API Keys*.

### "This address redirects to another host"

The address redirects elsewhere (an authentication proxy, another domain). Hangarr refuses to send the key there. Enter
the final address of the service.

### "This address answers as Radarr, not Sonarr."

The address belongs to another service. Check the address you entered: the same message format is used for every service.

### "This address answers as …, not Jellyfin."

The address is another media server, Emby for example. Hangarr does not work with Emby: the connection test refuses any
server whose name does not contain "Jellyfin".

### Hangarr in Docker cannot resolve `*.ts.net` names

Containers do not always use MagicDNS. To test:

```sh
docker run --rm curlimages/curl -sS https://sonarr.<tailnet>.ts.net/api/v3/system/status -H "X-Api-Key: <api-key>"
```

If name resolution fails, add `dns: [100.100.100.100]` to the `hangarr` service in `compose.yml`, or use the Tailscale
IP addresses (`100.x.y.z`).

### Tautulli webhook answers 401 (`bad token` in the logs)

The `X-Hangarr-Token` JSON header does not contain exactly the secret. Generate a new secret in Hangarr, and update
**both** triggers in Tautulli.

### Tautulli webhook answers 404

No webhook secret is configured, or Jellyfin is the media server. See [6](#6-tautulli-webhook).
