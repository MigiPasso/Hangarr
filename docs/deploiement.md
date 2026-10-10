# Déployer Hangarr

Ce guide couvre la publication de l'image, le déploiement avec **Docker** ou **Kubernetes**, la connexion à
Sonarr, Radarr, Plex (ou Jellyfin) et Tautulli, et les problèmes courants.

- [1. Avant de commencer](#1-avant-de-commencer)
- [2. Publier l'image sur GHCR](#2-publier-limage-sur-ghcr)
- [3. Déployer avec Docker](#3-déployer-avec-docker)
- [4. Déployer sur Kubernetes](#4-déployer-sur-kubernetes)
- [5. Première connexion](#5-première-connexion)
- [6. Connecter les services](#6-connecter-les-services)
- [7. Webhook Tautulli (recommandé)](#7-webhook-tautulli-recommandé)
- [8. Vérifier](#8-vérifier)
- [9. Exploitation : mise à jour, sauvegarde, mot de passe](#9-exploitation--mise-à-jour-sauvegarde-mot-de-passe)
- [10. Dépannage](#10-dépannage)

---

## 1. Avant de commencer

**Ce qu'il faut**
- Sonarr, Radarr, Plex ou Jellyfin joignables depuis l'endroit où tourne Hangarr, et Tautulli avec Plex.
- Leurs clés d'accès (voir [6](#6-connecter-les-services)).
- Docker (Compose ≥ 2.24) sur la machine, ou un cluster Kubernetes.

**Où l'installer**

| Situation | Conseil |
|---|---|
| Stack *arr* dans un cluster Kubernetes | Déployer Hangarr **dans le même namespace** : les services se joignent par leur nom court (`http://sonarr:8989`) et rien ne sort du cluster. |
| Stack *arr* en Docker sur un serveur | `compose.yml` sur la même machine, sur le même réseau Docker. |
| Test depuis un poste | `compose.yml` sur le poste, services joints via Tailscale (voir [10](#hangarr-en-docker-ne-résout-pas-les-noms-tsnet)). |

**Sécurité par défaut** : `DRY_RUN=true`. Toute suppression est simulée et journalisée, un bandeau orange le
rappelle. Ne passez à `false` qu'une fois les propositions de nettoyage vérifiées.

---

## 2. Publier l'image sur GHCR

À faire depuis le dépôt, une fois par version. Nécessaire pour Kubernetes (les nœuds doivent pouvoir récupérer
l'image) ; pour Docker, `docker compose up --build` construit l'image localement et cette étape est facultative.

1. **Token GitHub** : *Settings → Developer settings → Personal access tokens → Tokens (classic) → Generate new
   token (classic)*, scope **`write:packages`**. Les tokens « fine-grained » gèrent mal GHCR.
2. **Connexion** (nom d'utilisateur en minuscules) :
   ```sh
   echo "ghp_xxxxxxxx" | docker login ghcr.io -u <utilisateur> --password-stdin
   ```
3. **Version** : l'image prend le nom de `git describe`. Travaillez sur un arbre propre et taguez :
   ```sh
   git tag v0.1.0 && git push origin v0.1.0
   ```
4. **Construire et pousser** (amd64 + arm64, quelques minutes) :
   ```sh
   make image PUSH=1 IMAGE=ghcr.io/<utilisateur>/hangarr
   docker buildx imagetools inspect ghcr.io/<utilisateur>/hangarr:v0.1.0   # deux plateformes attendues
   ```
5. **Visibilité** : le paquet est **privé** au premier push.
   - Public : `https://github.com/<utilisateur>?tab=packages` → *hangarr* → *Package settings* → *Change
     visibility*. L'image ne contient aucun secret.
   - Privé : créez un secret de pull (token `read:packages`) et décommentez `imagePullSecrets` dans le manifest :
     ```sh
     kubectl -n <ns> create secret docker-registry ghcr-pull \
       --docker-server=ghcr.io --docker-username=<utilisateur> --docker-password=<token>
     ```

---

## 3. Déployer avec Docker

```sh
cp .env.example .env            # facultatif : DRY_RUN, COOKIE_SECURE, TZ, DEFAULT_LANG…
docker compose up -d --build    # ou HANGARR_IMAGE=ghcr.io/<utilisateur>/hangarr:v0.1.0 dans .env
docker compose logs hangarr | grep -A4 "INITIAL ADMIN"
```

Ouvrez `http://<hôte>:8080` (ou `http://localhost:8080`).

- **HTTP sur le réseau local** : mettez `COOKIE_SECURE=false` dans `.env`, sinon le navigateur refuse le cookie de
  session (la page de connexion le signale). Sur `localhost`, ce n'est pas nécessaire.
- **Derrière un reverse proxy HTTPS** : `COOKIE_SECURE=true` et l'adresse du proxy dans `TRUSTED_PROXIES`.
- **Même réseau que la stack *arr*** : décommentez le bloc `networks` de `compose.yml` (nom du réseau :
  `docker network ls`).
- **Clé maître (recommandé)** : `docker run --rm <image> master-key > master.key`, puis décommentez les blocs
  `secrets` de `compose.yml`. Sans elle, une clé est générée dans le volume (voir [9](#sauvegarde)).

`make deploy-check` démarre `compose.yml` tel quel contre de faux services et vérifie qu'il fonctionne.

---

## 4. Déployer sur Kubernetes

Fichier : [`deploy/kubernetes/hangarr.yaml`](../deploy/kubernetes/hangarr.yaml) (YAML simple, sans kustomize).
Il contient un ConfigMap, un PVC, un Deployment (une réplique, stratégie `Recreate`, non-root, système de fichiers
en lecture seule) et un Service. **Il n'impose aucun namespace** : utilisez celui de votre stack (ici `arr`).

### 4.1 Renseigner l'image

```yaml
image: ghcr.io/<utilisateur>/hangarr:v0.1.0
```

### 4.2 Créer le secret de la clé maître

Dans le **même namespace** que le Deployment, et jamais dans Git :

```sh
kubectl -n arr create secret generic hangarr-master-key \
  --from-literal=master-key="$(openssl rand -base64 32)"
```

Conservez cette clé (gestionnaire de mots de passe) : elle chiffre les clés d'API saisies dans l'interface. Perdue
ou changée, il faudra les ressaisir.

### 4.3 Appliquer

```sh
kubectl -n arr apply -f deploy/kubernetes/hangarr.yaml
kubectl -n arr get pods -l app.kubernetes.io/name=hangarr -w
```

**Avec Argo CD** : faites pointer l'application sur le fichier et choisissez le namespace de destination dans
l'application. Le secret, créé à la main, n'est pas géré par Argo CD, et c'est voulu.

### 4.4 Exposer l'application

Le Service écoute sur le port **8080**. Avec l'opérateur Tailscale, un Ingress HTTPS dans le tailnet :

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
        - hangarr        # → https://hangarr.<tailnet>.ts.net
```

Le Service doit sélectionner les pods par **le même label** que le Deployment :
`app.kubernetes.io/name: hangarr`. Sans exposition, `kubectl -n arr port-forward svc/hangarr 8080:8080` puis
`http://localhost:8080` suffit.

En HTTPS (Ingress Tailscale) ou via `port-forward` sur localhost, gardez `COOKIE_SECURE: "true"` dans le ConfigMap.

---

## 5. Première connexion

Le compte `admin` est créé au premier démarrage et son mot de passe est écrit **une seule fois** dans les logs :

```sh
kubectl -n arr logs deploy/hangarr | grep -A4 "INITIAL ADMIN"      # Kubernetes
docker compose logs hangarr | grep -A4 "INITIAL ADMIN"             # Docker
```

Si rien ne sort (le pod a été recréé depuis), générez un mot de passe temporaire :

```sh
kubectl -n arr exec deploy/hangarr -- /hangarr admin reset-password
docker compose exec hangarr /hangarr admin reset-password
```

À la connexion, l'application impose de choisir un nouveau mot de passe. L'interface est en anglais ; le lien
**FR** de la barre du haut la passe en français (`DEFAULT_LANG=fr` pour en faire la langue de l'instance).

**Variante : choisir le mot de passe à l'avance** avec `HANGARR_ADMIN_PASSWORD` (dans `.env` pour Docker ; pour
Kubernetes, en variable `env` du Deployment, lue depuis un Secret par `valueFrom.secretKeyRef`). Le compte est alors
créé avec ce mot de passe, rien n'est écrit dans les logs et aucun changement n'est imposé. Il doit respecter les
mêmes règles qu'un mot de passe saisi (12 caractères au moins, minuscule, majuscule, chiffre, symbole, sans le nom
d'utilisateur), sinon l'application refuse de démarrer, même si le compte existe déjà. La variable ne sert **qu'au
premier démarrage** : si le compte existe déjà, elle est ignorée (avertissement dans les logs) ; retirez-la une fois le
compte créé, elle reste sinon lisible dans la configuration du conteneur (`docker inspect`, spec du pod).

---

## 6. Connecter les services

Page **Configuration** (menu Réglages → Configuration). Pour chacun : adresse, clé, **Test connection**, puis **Save**.
La première synchronisation démarre seule dès que Sonarr, Radarr et le serveur multimédia (Plex ou Jellyfin, pas les deux)
sont enregistrés. Tautulli ne concerne que Plex.

L'« API URL » est **l'adresse de base** du service, celle de son interface web, sans `/api` : Hangarr ajoute le
reste. Incluez l'éventuelle *URL Base* (`https://hôte/sonarr`) et le port s'il n'est pas 443.

| Service | Adresse (même namespace Kubernetes) | Clé |
|---|---|---|
| Sonarr | `http://sonarr:8989` | *Settings → General → Security → API Key* |
| Radarr | `http://radarr:7878` | *Settings → General → Security → API Key* |
| Plex | `http://plex:32400` | token `X-Plex-Token` (ci-dessous) |
| Jellyfin | `http://jellyfin:8096` | *Tableau de bord → Clés API* (section [Jellyfin](#jellyfin)) |
| Tautulli | `http://tautulli:8181` | *Settings → Web Interface → API* (cocher *Enable API*) |

Les noms et ports exacts sont ceux de vos Services : `kubectl -n arr get svc`. Hors cluster : l'adresse que vous
ouvrez dans le navigateur (par exemple `https://sonarr.<tailnet>.ts.net`).

> Indiquez l'**adresse finale** : si une adresse redirige vers un autre hôte, Hangarr refuse pour ne pas y envoyer
> la clé, et le test l'explique.

### Plex : adresse du serveur

C'est le **Plex Media Server** (port 32400), pas `app.plex.tv`. Vérification, sans token :

```sh
curl -s http://plex:32400/identity     # petit XML avec machineIdentifier et version
```

Si Plex refuse le HTTP (*Paramètres → Réseau → Connexions sécurisées : Requises*) et que Hangarr n'est pas dans le
cluster :
- utilisez l'adresse HTTPS `plex.direct` du serveur :
  ```sh
  curl -s -H "Accept: application/json" \
    "https://plex.tv/api/v2/resources?includeHttps=1&X-Plex-Token=<token>" \
    | grep -o '"uri":"https://[^"]*plex.direct:32400"'
  ```
- ou un Ingress HTTPS Tailscale devant Plex,
- ou ajoutez le réseau d'où vient Hangarr dans *Réseau → Réseaux LAN* (`100.64.0.0/10` pour Tailscale : le trafic
  reste chiffré dans le tailnet, rien n'est exposé publiquement).

Le champ **Public URL** de Plex peut rester vide (liens « Ouvrir dans Plex » via app.plex.tv).

### Plex : token

Avec le **compte propriétaire du serveur** (un compte partagé ne voit que ses propres données) :

- Plex Web : un film ou un épisode → **⋯** → **Obtenir des infos** → **Afficher le XML** : l'URL se termine par
  `X-Plex-Token=xxxxxxxx`.
- Ou sur le serveur :
  ```sh
  kubectl -n arr exec deploy/plex -- grep -o 'PlexOnlineToken="[^"]*"' \
    "/config/Library/Application Support/Plex Media Server/Preferences.xml"
  ```

Ce token donne accès à tout le compte Plex : ne le partagez pas. Dans Hangarr il est chiffré et jamais réaffiché.
Il change si vous vous déconnectez de tous les appareils ou changez votre mot de passe Plex.

### Jellyfin

Jellyfin remplace Plex comme serveur multimédia : configurez l'un ou l'autre, pas les deux (voir le
[README](../README.md#plex-ou-jellyfin)). Tant que Plex est configuré, la carte Jellyfin est grisée : videz d'abord la
configuration de Plex. Sa configuration :

- **Adresse du serveur** : l'adresse que Hangarr joint, par exemple `http://jellyfin:8096` dans le même namespace.
- **Clé d'API** : *Tableau de bord → Clés API → +*. Comme les autres clés, elle est chiffrée en base.
- **URL publique** (facultative) : l'adresse que le navigateur ouvre pour les liens « Ouvrir dans Jellyfin ». Par défaut,
  l'adresse du serveur. Une adresse qui se termine par `/web` est refusée.

L'historique vient de Jellyfin lui-même, sans Tautulli (détails dans le README).

### Tautulli

Tautulli ne suit que Plex : avec Jellyfin, il n'est pas nécessaire (sa carte est alors grisée). Avec Plex, il est la
source de l'historique de lecture (vu / pas vu, par utilisateur). Une fois enregistré, lancez **Sync now** : la première
synchronisation importe tout l'historique, les suivantes seulement les nouveautés.

---

## 7. Webhook Tautulli (recommandé)

Sans webhook, les statistiques suivent la synchronisation planifiée (heure et fréquence réglables dans **Settings**). Avec, elles
sont à jour environ 30 secondes après chaque fin de lecture.

**Plex seulement** : Tautulli ne suit que Plex. Avec Jellyfin, il n'existe pas de webhook : la synchronisation planifiée
ou **Sync now** met l'historique à jour.

1. Hangarr : **Configuration → Tautulli → Webhook → Generate a secret**. Copiez-le : il n'est affiché **qu'une fois**.
2. Tautulli : *Settings → Notification Agents → Add a new notification agent → **Webhook***.
   - **Configuration** : *Webhook URL* `http://hangarr:8080/webhooks/tautulli` (même namespace ; sinon l'adresse
     sous laquelle Tautulli joint Hangarr), *Webhook Method* `POST`.
   - **Triggers** : **Playback Stop** et **Watched**.
   - **Data** : pour ces deux déclencheurs, *JSON Headers* :
     ```json
     {"X-Hangarr-Token": "<le secret>"}
     ```
   - **Save**.
3. Test : lancez puis arrêtez une lecture dans Plex.
   ```sh
   kubectl -n arr logs deploy/hangarr | grep -i webhook
   ```
   Attendu : `tautulli webhook received`, puis `history sync finished` une trentaine de secondes plus tard.

Le contenu envoyé par Tautulli est ignoré : l'appel ne fait que déclencher la lecture de l'historique par l'API.
Sans secret configuré, la route répond 404.

---

## 8. Vérifier

- **Health** : les services configurés « Reachable » (version, temps de réponse, alertes Sonarr/Radarr), l'historique
  des synchronisations et les écarts entre le serveur multimédia (Plex ou Jellyfin) et Sonarr/Radarr (éléments seulement
  dans ce serveur, rapprochements approximatifs…).
- **Series / Movies / Stats** : vos données.
- **Cleanup** : ce que personne n'a regardé depuis 180 jours (réglable dans **Settings**). Tant que `DRY_RUN=true`,
  les suppressions sont simulées et visibles dans **Log**.
- Pour protéger un élément : bouton **Protect** sur sa fiche (ajoute le tag `keep` dans Sonarr/Radarr).

---

## 9. Exploitation : mise à jour, sauvegarde, mot de passe

### Mise à jour

- Kubernetes : publiez une nouvelle version ([2](#2-publier-limage-sur-ghcr)), changez `image:` et appliquez /
  synchronisez. Les migrations de la base s'appliquent au démarrage.
- Docker : `git pull && docker compose up -d --build` (ou `docker compose pull && docker compose up -d` avec une
  image publiée).

### Sauvegarde

À chaud, copie cohérente de la base dans `/data/backups/` :

```sh
kubectl -n arr exec deploy/hangarr -- /hangarr backup
docker compose exec hangarr /hangarr backup
```

Conservez aussi la **clé maître** (le secret `hangarr-master-key`, ou `master.key` dans le volume sans clé
fournie) : sans elle, les clés d'API enregistrées sont illisibles. Restauration : arrêter, remplacer
`/data/hangarr.db` par la copie, redémarrer.

### Mot de passe perdu

```sh
kubectl -n arr exec deploy/hangarr -- /hangarr admin reset-password
```

---

## 10. Dépannage

### `secret "hangarr-master-key" not found`

Le secret n'existe pas **dans le namespace du pod**. Créez-le ([4.2](#42-créer-le-secret-de-la-clé-maître)) :
le kubelet réessaie seul, inutile de redéployer.

### `namespaces "hangarr" not found` (Argo CD)

Une ressource porte un `namespace:` qui n'existe pas. Le manifest fourni n'en impose aucun : retirez toute ligne
`namespace:` de votre copie et laissez Argo CD utiliser celui de l'application.

### 502 Bad Gateway sur l'Ingress

Le proxy ne joint pas le pod.

```sh
kubectl -n arr get pods -l app.kubernetes.io/name=hangarr
kubectl -n arr get endpoints hangarr        # doit lister IP:8080
```

- Endpoints vides : le `selector` du Service ne correspond pas aux labels du pod
  (`app.kubernetes.io/name: hangarr`).
- Pod non prêt : `kubectl -n arr describe pod -l app.kubernetes.io/name=hangarr` (événements en bas) et
  `kubectl -n arr logs deploy/hangarr`.
- Backend de l'Ingress sur le port 80 au lieu de **8080**.

### La connexion échoue sans message en HTTP

`COOKIE_SECURE=true` alors que la page est servie en HTTP (hors localhost) : le navigateur refuse le cookie. Passez
en HTTPS ou mettez `COOKIE_SECURE=false` pour un test local. La page de connexion affiche un avertissement.

### « Access denied (HTTP 401) » au test de connexion

Mauvaise clé d'API ou mauvais token. Pour Plex, utilisez le token du compte propriétaire. Pour Jellyfin, utilisez une clé
d'API créée dans *Tableau de bord → Clés API*.

### « This address redirects to another host »

L'adresse redirige ailleurs (proxy d'authentification, autre domaine). Hangarr refuse pour ne pas envoyer la clé à
un autre hôte : indiquez l'adresse finale du service.

### « This address answers as Radarr, not Sonarr »

L'adresse saisie est celle d'un autre service.

### « This address answers as …, not Jellyfin »

L'adresse est celle d'un autre serveur, Emby par exemple : Hangarr ne fonctionne pas avec Emby. Le test de connexion
refuse tout serveur dont le nom ne contient pas « Jellyfin ».

### Hangarr en Docker ne résout pas les noms `*.ts.net`

Un conteneur ne passe pas toujours par MagicDNS. Test :

```sh
docker run --rm curlimages/curl -sS https://sonarr.<tailnet>.ts.net/api/v3/system/status -H "X-Api-Key: <clé>"
```

En cas d'erreur de résolution, ajoutez `dns: [100.100.100.100]` au service `hangarr` de `compose.yml`, ou utilisez
les IP Tailscale (`100.x.y.z`).

### Le webhook Tautulli répond 401 (`bad token` dans les logs)

L'en-tête JSON ne contient pas exactement le secret. Générez-en un nouveau et mettez à jour les deux déclencheurs
dans Tautulli.

---

## 11. Migrer depuis Cleanarr

Hangarr s'appelait **Cleanarr**. Une installation existante se migre sans rien ressaisir.

**Ce que Hangarr reprend tout seul**

- la base `cleanarr.db` est renommée `hangarr.db` au premier démarrage (avec ses fichiers `-wal` / `-shm`) ;
- `CLEANARR_MASTER_KEY` / `CLEANARR_MASTER_KEY_FILE` sont encore lues, avec un avertissement dans les logs :
  renommez-les `HANGARR_MASTER_KEY` / `HANGARR_MASTER_KEY_FILE`. La clé ne change pas, les clés d'API enregistrées
  restent lisibles ;
- l'en-tête `X-Cleanarr-Token` du webhook Tautulli est encore accepté (avertissement dans les logs) : remplacez-le
  par `X-Hangarr-Token` dans les deux déclencheurs Tautulli.

Les cookies ont changé de nom : il faut **se reconnecter une fois**, et rechoisir langue, thème et menu réduit.

**Avant tout** : une sauvegarde, avec l'ancienne image.

```sh
docker compose -p cleanarr exec cleanarr /cleanarr backup        # Docker
kubectl -n arr exec deploy/cleanarr -- /cleanarr backup          # Kubernetes
```

### Docker

Le projet Compose s'appelle maintenant `hangarr` : son volume est `hangarr_hangarr-data`, il faut y copier les
données de `cleanarr_cleanarr-data` (l'ancien volume reste intact, retour arrière possible).

```sh
docker compose -p cleanarr down                       # SANS -v : le volume est conservé
docker volume ls | grep cleanarr                      # nom exact de l'ancien volume
docker volume create hangarr_hangarr-data
docker run --rm -v cleanarr_cleanarr-data:/from:ro -v hangarr_hangarr-data:/to alpine cp -a /from/. /to/
```

Dans `.env`, renommez `CLEANARR_IMAGE`, `CLEANARR_PORT` et `CLEANARR_MASTER_KEY(_FILE)` en `HANGARR_…` (les deux
premiers ne sont plus lus : sans renommage, l'image et le port reprennent leur valeur par défaut). Puis :

```sh
docker compose up -d --build
docker compose logs hangarr | grep -i deprecated      # rien ne doit sortir
```

Une fois Hangarr vérifié : `docker volume rm cleanarr_cleanarr-data`.

### Kubernetes

Principe : copier les données de l'ancien PVC `cleanarr-data` dans le nouveau `hangarr-data`, avec **la même clé
maître**, puis déployer `hangarr.yaml` tel quel. L'ancien PVC n'est touché qu'à la fin, une fois Hangarr vérifié :
jusque-là, retour arrière immédiat. Les commandes supposent le namespace `arr` ; adaptez `-n`.

**0. Prérequis.** L'image `ghcr.io/<user>/hangarr:<version>` est publiée ([2](#2-publier-limage-sur-ghcr)) et
renseignée dans `hangarr.yaml` (`image:`), comme vous l'aviez fait pour Cleanarr.

**1. Avec Argo CD : couper la synchronisation automatique** de l'application Cleanarr (UI : *App details › Sync
policy › Disable auto-sync*, ou `argocd app set <app> --sync-policy none`). Sinon, dès que l'application suivra
`hangarr.yaml`, Argo CD supprimerait (*prune*) l'ancien PVC, donc vos données.

**2. Sauvegarde** (copie cohérente dans `/data/backups/`, sur le PVC lui-même, copiée avec le reste) :

```sh
kubectl -n arr exec deploy/cleanarr -- /cleanarr backup
```

**3. Même clé maître, nouveau nom de secret.** Indispensable : avec une autre clé, les clés d'API enregistrées
seraient illisibles.

```sh
kubectl -n arr create secret generic hangarr-master-key --from-literal=master-key="$(
  kubectl -n arr get secret cleanarr-master-key -o jsonpath='{.data.master-key}' | base64 -d)"
```

**4. Arrêter Cleanarr** (une seule application à la fois sur la base SQLite) :

```sh
kubectl -n arr scale deploy/cleanarr --replicas=0
kubectl -n arr wait --for=delete pod -l app.kubernetes.io/name=cleanarr --timeout=60s
```

**5. Créer le nouveau PVC et y copier les données.** Même taille et même `storageClassName` que dans
`hangarr.yaml`. Le pod de copie respecte le profil `restricted` (utilisateur 65532, celui de l'application) :

```sh
kubectl -n arr apply -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hangarr-data
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 5Gi
  # storageClassName: <votre-storage-class>
---
apiVersion: v1
kind: Pod
metadata:
  name: hangarr-migrate
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: copy
      image: busybox:1.36
      command: ["sh", "-c", "cp -a /from/. /to/ && ls -la /to"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: [ALL] }
      volumeMounts:
        - { name: from, mountPath: /from, readOnly: true }
        - { name: to, mountPath: /to }
  volumes:
    - name: from
      persistentVolumeClaim: { claimName: cleanarr-data, readOnly: true }
    - name: to
      persistentVolumeClaim: { claimName: hangarr-data }
YAML
kubectl -n arr wait --for=jsonpath='{.status.phase}'=Succeeded pod/hangarr-migrate --timeout=180s
kubectl -n arr logs pod/hangarr-migrate        # doit lister cleanarr.db (et master.key / cache s'il y en a)
kubectl -n arr delete pod/hangarr-migrate
```

**6. Déployer Hangarr.**

- Avec Argo CD : dans l'application, remplacer l'URL du dépôt (`…/<owner>/cleanarr.git` → `…/<owner>/Hangarr.git`,
  l'ancienne ne répond plus) et le chemin (`deploy/kubernetes/cleanarr.yaml` → `deploy/kubernetes/hangarr.yaml`),
  puis synchroniser **sans** *prune* pour l'instant.
- Sans Argo CD : `kubectl -n arr apply -f deploy/kubernetes/hangarr.yaml`.

**7. Vérifier**, avant de supprimer quoi que ce soit :

```sh
kubectl -n arr logs deploy/hangarr | grep -iE "renamed|INITIAL ADMIN|deprecated"
```

- attendu : `database renamed after the rename of Cleanarr to Hangarr` ;
- **`INITIAL ADMIN` ne doit pas apparaître** : sinon Hangarr est parti sur un volume vide (copie ratée), revenir en
  arrière (ci-dessous) et recommencer l'étape 5 ;
- connexion avec votre mot de passe habituel (les cookies ont changé : une reconnexion), page Configuration : les
  services sont là, « Tester la connexion » répond.

**8. Adapter ce qui citait `cleanarr`** : l'Ingress ou le nom d'hôte Tailscale (annotation
`tailscale.com/hostname` du Service), et dans Tautulli l'URL du webhook
(`http://hangarr.<ns>.svc.cluster.local:8080/webhooks/tautulli`) ainsi que l'en-tête, `X-Hangarr-Token`.

**9. Nettoyer**, une fois satisfait : réactiver la synchronisation automatique d'Argo CD (le *prune* supprimera alors
les objets `cleanarr*` qui ne sont plus dans le manifeste), ou à la main :

```sh
kubectl -n arr delete deploy/cleanarr svc/cleanarr cm/cleanarr-config secret/cleanarr-master-key pvc/cleanarr-data
```

**Retour arrière** (tant que l'étape 9 n'est pas faite) : l'ancien PVC n'a pas été modifié.

```sh
kubectl -n arr scale deploy/hangarr --replicas=0
kubectl -n arr scale deploy/cleanarr --replicas=1
```

(avec Argo CD, remettre d'abord l'ancienne configuration de l'application, sans quoi elle ramènerait Hangarr).

### Dépôt et image

Renommer le dépôt Forgejo (et le dossier local) en `hangarr` se fait de votre côté ; l'image se publie désormais
sous `ghcr.io/<user>/hangarr` ([2](#2-publier-limage-sur-ghcr)).
