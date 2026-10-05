# Cleanarr

Application Go + HTMX auto-hébergée qui croise Tautulli, Plex, Sonarr et Radarr pour savoir ce qui est
regardé, par qui, ce qui ne l'est jamais, et faire le ménage proprement. Spécification complète : [SPEC.md](SPEC.md).

> État : phases 1 à 5 livrées (authentification, synchronisation, bibliothèque, fiches et suppressions, tableau de
> bord, statistiques, nettoyage) ; **phase 6 en cours** (configuration dans l'UI, santé, webhook, déploiement Docker).

## Prérequis

Uniquement **Docker** (Engine ≥ 24 recommandé) avec **Compose v2** (≥ 2.24). Aucun Go, Node, linter ou
navigateur n'est nécessaire sur la machine : tout tourne dans des conteneurs.

## Développement

| Cible `make` | Commande `docker compose` équivalente |
|---|---|
| `make dev` | `docker compose -f compose.dev.yml up --build app mock` |
| `make dev-real` | `CLEANARR_ENV_FILE=.env docker compose -f compose.dev.yml --profile real up --build app-real` |
| `make test` | `docker compose -f compose.dev.yml --profile tools run --rm test` (après `build test`) |
| `make lint` | `docker compose -f compose.dev.yml --profile tools run --rm lint` (après `build lint`) |
| `make e2e` | voir le `Makefile` : démarre `app-e2e` + `mock`, lit le mot de passe initial dans les logs, lance `e2e`, arrête tout |
| `make deps` | `docker compose -f compose.dev.yml --profile tools run --rm gotools go mod tidy` |
| `make image-load` | `docker build --target runtime -t cleanarr:local .` |
| `make image` | `docker buildx build --platform linux/amd64,linux/arm64 --target runtime …` (`PUSH=1` pour pousser) |
| `make reset-admin` | `docker compose -f compose.dev.yml exec app cleanarr admin reset-password` |
| `make sync` | `docker compose -f compose.dev.yml exec app cleanarr sync --once` |
| `make deploy-check` | valide `compose.yml` puis le démarre tel quel contre le mock (projet séparé) et lance `deploy/check.sh` |
| `make capture-fixtures` | `CLEANARR_ENV_FILE=.env docker compose -f compose.dev.yml --profile real run --rm capture` |
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
 CLEANARR: INITIAL ADMIN ACCOUNT CREATED
 Username : admin
 Password : …
 ...
```

En dev : `docker compose -f compose.dev.yml logs app | grep -A4 "INITIAL ADMIN"`. Le changement est obligatoire à la
première connexion. Mot de passe perdu : `make reset-admin` (ou `cleanarr admin reset-password` via
`docker exec` / `kubectl exec`), qui affiche un nouveau mot de passe temporaire uniquement dans la sortie de la commande.

## Récupérer le token Plex et les clés d'API

- **Sonarr / Radarr** : *Settings → General → Security → API Key*.
- **Tautulli** : *Settings → Web Interface → API* (cocher *Enable API*), copier l'*API key*.
- **Plex** : ouvrir un média dans Plex Web → *⋯ → Obtenir des infos → Afficher le XML* ; l'URL contient
  `X-Plex-Token=…`. Voir aussi la documentation officielle « Finding an authentication token / X-Plex-Token ».

## Déploiement avec Docker

Le fichier [`compose.yml`](compose.yml) est prêt à l'emploi : image durcie (distroless, utilisateur 65532, système de
fichiers en lecture seule, aucune capacité), un volume pour `/data`, redémarrage automatique, healthcheck intégré.

```sh
cp .env.example .env            # facultatif : DRY_RUN, COOKIE_SECURE, TZ…
docker compose up -d --build
docker compose logs cleanarr | grep -A4 "INITIAL ADMIN"   # mot de passe initial, affiché une seule fois
```

Ouvrir `http://<hôte>:8080`, changer le mot de passe, puis configurer Sonarr, Radarr, Plex et Tautulli dans
**Configuration** (bouton « Tester la connexion »). La première synchronisation part dès que la configuration est complète.

- **Sans HTTPS** (test sur le réseau local) : mettre `COOKIE_SECURE=false` dans `.env`, sinon le navigateur refuse le
  cookie de session (la page de connexion le signale). Derrière un reverse proxy HTTPS, remettre `true` et déclarer
  l'adresse du proxy dans `TRUSTED_PROXIES`.
- **Même réseau que la stack *arr*** : décommenter le bloc `networks` de `compose.yml` (nom du réseau : `docker network
  ls`). Sonarr se joint alors par `http://sonarr:8989`, et Tautulli appelle le webhook par
  `http://cleanarr:8080/webhooks/tautulli`.
- **Clé maître (recommandé)** : `docker run --rm cleanarr:local master-key > master.key`, puis décommenter les blocs
  `secrets` de `compose.yml`. Sans elle, la clé est générée dans le volume (voir « Configuration des services »).
- **Équivalent `docker run`** :
  ```sh
  docker run -d --name cleanarr --restart unless-stopped -p 8080:8080 --env-file .env \
    -v cleanarr-data:/data --read-only --user 65532:65532 --cap-drop ALL \
    --security-opt no-new-privileges:true cleanarr:local
  ```
- **Dossier de l'hôte au lieu d'un volume** (`-v /srv/cleanarr:/data`) : il doit appartenir à l'utilisateur du
  conteneur : `sudo chown -R 65532:65532 /srv/cleanarr`.
- **Mise à jour** : `git pull && docker compose up -d --build` (plus tard, avec l'image publiée :
  `CLEANARR_IMAGE=ghcr.io/<user>/cleanarr:latest`, puis `docker compose pull && docker compose up -d`). Les migrations
  de la base s'appliquent au démarrage.
- **Sauvegarde**, à chaud : `docker compose exec cleanarr /cleanarr backup` écrit une copie cohérente dans
  `/data/backups/cleanarr-AAAAMMJJ-HHMMSS.db` (ou `… backup /data/backups/mon-fichier.db`). Les clés d'API enregistrées
  y sont chiffrées : conserver aussi la clé maître (`master.key` du volume, ou `CLEANARR_MASTER_KEY`). Pour restaurer :
  arrêter le conteneur, remplacer `/data/cleanarr.db` par la copie, redémarrer.
- **Mot de passe admin perdu** : `docker compose exec cleanarr /cleanarr admin reset-password`.

## Kubernetes

Manifests YAML simples dans [`deploy/kubernetes/cleanarr.yaml`](deploy/kubernetes/cleanarr.yaml) : Namespace,
ConfigMap, PVC, Deployment (une réplique, stratégie `Recreate`, non-root, rootfs en lecture seule, sondes
`/healthz` et `/readyz`) et Service. Les étapes sont en tête du fichier :

1. publier l'image dans un registre que les nœuds peuvent joindre (voir ci-dessous) et renseigner `image:` ;
2. créer le secret de la clé maître : `kubectl -n cleanarr create secret generic cleanarr-master-key
   --from-literal=master-key="$(docker run --rm <image> master-key)"` ;
3. `kubectl apply -f deploy/kubernetes/cleanarr.yaml`, puis
   `kubectl -n cleanarr logs deploy/cleanarr | grep -A4 "INITIAL ADMIN"` pour le mot de passe initial ;
4. configurer les services dans l'UI avec leurs adresses internes au cluster
   (`http://sonarr.<namespace>.svc.cluster.local:8989`…).

**Image** : `make image PUSH=1 IMAGE=ghcr.io/<utilisateur>/cleanarr` construit amd64 + arm64 et pousse (après
`docker login ghcr.io` avec un token `write:packages`). Un paquet GHCR est privé par défaut : le rendre public, ou créer
un secret `docker-registry` et décommenter `imagePullSecrets`.

## Langue

L'interface est en anglais par défaut, et en français avec le lien **FR** de la barre du haut (choix retenu par le
navigateur). `DEFAULT_LANG=fr` fait démarrer une instance en français.

## Configuration des services

Sonarr, Radarr, Plex et Tautulli se configurent dans la page **Configuration** (URL, URL publique, clé d'API ou token,
bouton « Tester la connexion »). Sans configuration, l'application démarre quand même : la synchronisation est
désactivée et un bandeau invite à configurer. La première synchronisation part dès que la configuration est complète.

- **Clé maître** : les clés et tokens sont chiffrés en base (AES-256-GCM).
  - *Recommandé* : fournir une clé de 32 octets par `CLEANARR_MASTER_KEY` ou `CLEANARR_MASTER_KEY_FILE` (secret Docker
    ou Kubernetes), conservée hors du volume de données. Pour en générer une : `docker run --rm <image> master-key`.
  - *Par défaut* : sans clé fournie, une clé est générée au premier démarrage dans `/data/master.key`. Ça marche sans
    rien préparer, mais une sauvegarde ou une copie du volume expose alors les secrets ; la page Configuration le signale.
  - Passer du mode par défaut au mode recommandé : définir la variable et redémarrer. Les secrets sont rechiffrés avec la
    nouvelle clé et `master.key` est supprimé, sans rien ressaisir. Changer ensuite de clé fournie rend en revanche les
    secrets illisibles : ils sont signalés et doivent être ressaisis.
- **Variables d'environnement prioritaires** : `SONARR_URL`, `SONARR_API_KEY`, `PLEX_TOKEN`… restent acceptées. Une
  valeur définie dans l'environnement est utilisée telle quelle et verrouillée dans l'UI.
- Les secrets ne sont jamais renvoyés au navigateur (seuls `••••` et les 4 derniers caractères s'affichent). Un champ
  laissé vide conserve la valeur actuelle. Chaque modification est inscrite dans le journal (onglet Sécurité).

## Webhook Tautulli (recommandé)

Sans webhook, les statistiques suivent la synchronisation quotidienne. Avec, elles sont à jour environ 30 secondes après
chaque fin de lecture : Tautulli prévient Cleanarr, qui relit seulement l'historique.

1. Configuration → Tautulli → « Générer un secret » (affiché une seule fois), ou `TAUTULLI_WEBHOOK_SECRET` (32 caractères min.).
2. Tautulli → Settings → Notification Agents → Webhook : URL `http://<cleanarr>:8080/webhooks/tautulli`, méthode `POST`,
   déclencheurs « Playback Stop » et « Watched », et dans l'onglet Data, *JSON Headers* :
   `{"X-Cleanarr-Token": "<secret>"}` pour ces deux déclencheurs.

Le contenu envoyé par Tautulli est ignoré (l'appel ne fait que déclencher la lecture de l'historique par l'API) ; sans
secret, la route répond 404.

## Configuration

Toutes les variables sont décrites dans [SPEC.md §10](SPEC.md#10-configuration-variables-denvironnement).
Écarts : pas de score de nettoyage ni de `GRACE_DAYS` : la page Nettoyage propose ce que personne n'a regardé depuis
N jours (jamais vu : depuis l'ajout), N réglable dans Réglages (180 par défaut) ; `SYNC_INTERVAL` est remplacé par `SYNC_TIME` (heure de la synchronisation quotidienne, défaut `04:00`,
modifiable ensuite dans la page Réglages) ; les variables `PROWLARR_*` n'existent pas.
Ajouts : `SONARR_PUBLIC_URL`, `RADARR_PUBLIC_URL` et `PLEX_PUBLIC_URL` (optionnels) donnent les adresses utilisées par les
liens « Ouvrir dans Sonarr/Radarr/Plex » quand les URL d'API ne sont pas joignables depuis le navigateur (Service
Kubernetes interne). Par défaut : les URL d'API, et app.plex.tv pour Plex.
