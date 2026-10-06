# Déployer Cleanarr

Ce guide couvre la publication de l'image, le déploiement avec **Docker** ou **Kubernetes**, la connexion à
Sonarr, Radarr, Plex et Tautulli, et les problèmes courants.

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
- Sonarr, Radarr, Plex et Tautulli joignables depuis l'endroit où tourne Cleanarr.
- Leurs clés d'accès (voir [6](#6-connecter-les-services)).
- Docker (Compose ≥ 2.24) sur la machine, ou un cluster Kubernetes.

**Où l'installer**

| Situation | Conseil |
|---|---|
| Stack *arr* dans un cluster Kubernetes | Déployer Cleanarr **dans le même namespace** : les services se joignent par leur nom court (`http://sonarr:8989`) et rien ne sort du cluster. |
| Stack *arr* en Docker sur un serveur | `compose.yml` sur la même machine, sur le même réseau Docker. |
| Test depuis un poste | `compose.yml` sur le poste, services joints via Tailscale (voir [10](#cleanarr-en-docker-ne-résout-pas-les-noms-tsnet)). |

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
   make image PUSH=1 IMAGE=ghcr.io/<utilisateur>/cleanarr
   docker buildx imagetools inspect ghcr.io/<utilisateur>/cleanarr:v0.1.0   # deux plateformes attendues
   ```
5. **Visibilité** : le paquet est **privé** au premier push.
   - Public : `https://github.com/<utilisateur>?tab=packages` → *cleanarr* → *Package settings* → *Change
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
docker compose up -d --build    # ou CLEANARR_IMAGE=ghcr.io/<utilisateur>/cleanarr:v0.1.0 dans .env
docker compose logs cleanarr | grep -A4 "INITIAL ADMIN"
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

Fichier : [`deploy/kubernetes/cleanarr.yaml`](../deploy/kubernetes/cleanarr.yaml) (YAML simple, sans kustomize).
Il contient un ConfigMap, un PVC, un Deployment (une réplique, stratégie `Recreate`, non-root, système de fichiers
en lecture seule) et un Service. **Il n'impose aucun namespace** : utilisez celui de votre stack (ici `arr`).

### 4.1 Renseigner l'image

```yaml
image: ghcr.io/<utilisateur>/cleanarr:v0.1.0
```

### 4.2 Créer le secret de la clé maître

Dans le **même namespace** que le Deployment, et jamais dans Git :

```sh
kubectl -n arr create secret generic cleanarr-master-key \
  --from-literal=master-key="$(openssl rand -base64 32)"
```

Conservez cette clé (gestionnaire de mots de passe) : elle chiffre les clés d'API saisies dans l'interface. Perdue
ou changée, il faudra les ressaisir.

### 4.3 Appliquer

```sh
kubectl -n arr apply -f deploy/kubernetes/cleanarr.yaml
kubectl -n arr get pods -l app.kubernetes.io/name=cleanarr -w
```

**Avec Argo CD** : faites pointer l'application sur le fichier et choisissez le namespace de destination dans
l'application. Le secret, créé à la main, n'est pas géré par Argo CD, et c'est voulu.

### 4.4 Exposer l'application

Le Service écoute sur le port **8080**. Avec l'opérateur Tailscale, un Ingress HTTPS dans le tailnet :

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: cleanarr
spec:
  ingressClassName: tailscale
  defaultBackend:
    service:
      name: cleanarr
      port:
        number: 8080
  tls:
    - hosts:
        - cleanarr        # → https://cleanarr.<tailnet>.ts.net
```

Le Service doit sélectionner les pods par **le même label** que le Deployment :
`app.kubernetes.io/name: cleanarr`. Sans exposition, `kubectl -n arr port-forward svc/cleanarr 8080:8080` puis
`http://localhost:8080` suffit.

En HTTPS (Ingress Tailscale) ou via `port-forward` sur localhost, gardez `COOKIE_SECURE: "true"` dans le ConfigMap.

---

## 5. Première connexion

Le compte `admin` est créé au premier démarrage et son mot de passe est écrit **une seule fois** dans les logs :

```sh
kubectl -n arr logs deploy/cleanarr | grep -A4 "INITIAL ADMIN"      # Kubernetes
docker compose logs cleanarr | grep -A4 "INITIAL ADMIN"             # Docker
```

Si rien ne sort (le pod a été recréé depuis), générez un mot de passe temporaire :

```sh
kubectl -n arr exec deploy/cleanarr -- /cleanarr admin reset-password
docker compose exec cleanarr /cleanarr admin reset-password
```

À la connexion, l'application impose de choisir un nouveau mot de passe. L'interface est en anglais ; le lien
**FR** de la barre du haut la passe en français (`DEFAULT_LANG=fr` pour en faire la langue de l'instance).

---

## 6. Connecter les services

Page **Services** (« Configuration » en français). Pour chacun : adresse, clé, **Test connection**, puis **Save**.
La première synchronisation démarre seule dès que Sonarr, Radarr, Plex et Tautulli sont enregistrés.

L'« API URL » est **l'adresse de base** du service, celle de son interface web, sans `/api` : Cleanarr ajoute le
reste. Incluez l'éventuelle *URL Base* (`https://hôte/sonarr`) et le port s'il n'est pas 443.

| Service | Adresse (même namespace Kubernetes) | Clé |
|---|---|---|
| Sonarr | `http://sonarr:8989` | *Settings → General → Security → API Key* |
| Radarr | `http://radarr:7878` | *Settings → General → Security → API Key* |
| Plex | `http://plex:32400` | token `X-Plex-Token` (ci-dessous) |
| Tautulli | `http://tautulli:8181` | *Settings → Web Interface → API* (cocher *Enable API*) |

Les noms et ports exacts sont ceux de vos Services : `kubectl -n arr get svc`. Hors cluster : l'adresse que vous
ouvrez dans le navigateur (par exemple `https://sonarr.<tailnet>.ts.net`).

> Indiquez l'**adresse finale** : si une adresse redirige vers un autre hôte, Cleanarr refuse pour ne pas y envoyer
> la clé, et le test l'explique.

### Plex : adresse du serveur

C'est le **Plex Media Server** (port 32400), pas `app.plex.tv`. Vérification, sans token :

```sh
curl -s http://plex:32400/identity     # petit XML avec machineIdentifier et version
```

Si Plex refuse le HTTP (*Paramètres → Réseau → Connexions sécurisées : Requises*) et que Cleanarr n'est pas dans le
cluster :
- utilisez l'adresse HTTPS `plex.direct` du serveur :
  ```sh
  curl -s -H "Accept: application/json" \
    "https://plex.tv/api/v2/resources?includeHttps=1&X-Plex-Token=<token>" \
    | grep -o '"uri":"https://[^"]*plex.direct:32400"'
  ```
- ou un Ingress HTTPS Tailscale devant Plex,
- ou ajoutez le réseau d'où vient Cleanarr dans *Réseau → Réseaux LAN* (`100.64.0.0/10` pour Tailscale : le trafic
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

Ce token donne accès à tout le compte Plex : ne le partagez pas. Dans Cleanarr il est chiffré et jamais réaffiché.
Il change si vous vous déconnectez de tous les appareils ou changez votre mot de passe Plex.

### Tautulli

Tautulli est la source de l'historique de lecture (vu / pas vu, par utilisateur). Une fois enregistré, lancez
**Sync now** : la première synchronisation importe tout l'historique, les suivantes seulement les nouveautés.

---

## 7. Webhook Tautulli (recommandé)

Sans webhook, les statistiques suivent la synchronisation planifiée (heure et fréquence réglables dans **Settings**). Avec, elles
sont à jour environ 30 secondes après chaque fin de lecture.

1. Cleanarr : **Services → Tautulli → Webhook → Generate a secret**. Copiez-le : il n'est affiché **qu'une fois**.
2. Tautulli : *Settings → Notification Agents → Add a new notification agent → **Webhook***.
   - **Configuration** : *Webhook URL* `http://cleanarr:8080/webhooks/tautulli` (même namespace ; sinon l'adresse
     sous laquelle Tautulli joint Cleanarr), *Webhook Method* `POST`.
   - **Triggers** : **Playback Stop** et **Watched**.
   - **Data** : pour ces deux déclencheurs, *JSON Headers* :
     ```json
     {"X-Cleanarr-Token": "<le secret>"}
     ```
   - **Save**.
3. Test : lancez puis arrêtez une lecture dans Plex.
   ```sh
   kubectl -n arr logs deploy/cleanarr | grep -i webhook
   ```
   Attendu : `tautulli webhook received`, puis `history sync finished` une trentaine de secondes plus tard.

Le contenu envoyé par Tautulli est ignoré : l'appel ne fait que déclencher la lecture de l'historique par l'API.
Sans secret configuré, la route répond 404.

---

## 8. Vérifier

- **Health** : les quatre services « Reachable » (version, temps de réponse, alertes Sonarr/Radarr), l'historique
  des synchronisations et les écarts entre Plex et Sonarr/Radarr (éléments seulement dans Plex, rapprochements
  approximatifs…).
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
kubectl -n arr exec deploy/cleanarr -- /cleanarr backup
docker compose exec cleanarr /cleanarr backup
```

Conservez aussi la **clé maître** (le secret `cleanarr-master-key`, ou `master.key` dans le volume sans clé
fournie) : sans elle, les clés d'API enregistrées sont illisibles. Restauration : arrêter, remplacer
`/data/cleanarr.db` par la copie, redémarrer.

### Mot de passe perdu

```sh
kubectl -n arr exec deploy/cleanarr -- /cleanarr admin reset-password
```

---

## 10. Dépannage

### `secret "cleanarr-master-key" not found`

Le secret n'existe pas **dans le namespace du pod**. Créez-le ([4.2](#42-créer-le-secret-de-la-clé-maître)) :
le kubelet réessaie seul, inutile de redéployer.

### `namespaces "cleanarr" not found` (Argo CD)

Une ressource porte un `namespace:` qui n'existe pas. Le manifest fourni n'en impose aucun : retirez toute ligne
`namespace:` de votre copie et laissez Argo CD utiliser celui de l'application.

### 502 Bad Gateway sur l'Ingress

Le proxy ne joint pas le pod.

```sh
kubectl -n arr get pods -l app.kubernetes.io/name=cleanarr
kubectl -n arr get endpoints cleanarr        # doit lister IP:8080
```

- Endpoints vides : le `selector` du Service ne correspond pas aux labels du pod
  (`app.kubernetes.io/name: cleanarr`).
- Pod non prêt : `kubectl -n arr describe pod -l app.kubernetes.io/name=cleanarr` (événements en bas) et
  `kubectl -n arr logs deploy/cleanarr`.
- Backend de l'Ingress sur le port 80 au lieu de **8080**.

### La connexion échoue sans message en HTTP

`COOKIE_SECURE=true` alors que la page est servie en HTTP (hors localhost) : le navigateur refuse le cookie. Passez
en HTTPS ou mettez `COOKIE_SECURE=false` pour un test local. La page de connexion affiche un avertissement.

### « Access denied (HTTP 401) » au test de connexion

Mauvaise clé d'API ou mauvais token. Pour Plex, utilisez le token du compte propriétaire.

### « This address redirects to another host »

L'adresse redirige ailleurs (proxy d'authentification, autre domaine). Cleanarr refuse pour ne pas envoyer la clé à
un autre hôte : indiquez l'adresse finale du service.

### « This address answers as Radarr, not Sonarr »

L'adresse saisie est celle d'un autre service.

### Cleanarr en Docker ne résout pas les noms `*.ts.net`

Un conteneur ne passe pas toujours par MagicDNS. Test :

```sh
docker run --rm curlimages/curl -sS https://sonarr.<tailnet>.ts.net/api/v3/system/status -H "X-Api-Key: <clé>"
```

En cas d'erreur de résolution, ajoutez `dns: [100.100.100.100]` au service `cleanarr` de `compose.yml`, ou utilisez
les IP Tailscale (`100.x.y.z`).

### Le webhook Tautulli répond 401 (`bad token` dans les logs)

L'en-tête JSON ne contient pas exactement le secret. Générez-en un nouveau et mettez à jour les deux déclencheurs
dans Tautulli.
