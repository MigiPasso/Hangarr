# Jellyfin lab

A real Jellyfin server with the scenario of `testdata/jellyfin/`, to check the Jellyfin client against an actual server.

- `make jellyfin-lab`: starts it on http://localhost:8096 (`LAB_PORT` changes the port) and seeds it.
- `make jellyfin-lab-capture`: records its anonymized responses into `testdata/captured/jellyfin/`. Review them before committing.
- `make jellyfin-lab-down`: removes the containers and volumes.

Admin account: `admin` / `hangarr-lab-admin`. The other accounts (`alice`, `bob`, `carol`) use `hangarr-lab-user`.
The Jellyfin version is the image tag in `compose.yml`; change it there to test another release.

The lab only listens on localhost. To use it from the dev app, start the dev app without a media server from its
environment: `MEDIA=none make dev`. Then attach the app to the lab network with
`docker network connect hangarr-jellyfin-lab_default hangarr-dev-app-1`, and configure Jellyfin on the Services page
(Settings → Services; « Configuration » in the French interface) with `http://jellyfin:8096` and the API key printed by
`docker compose -f deploy/jellyfin-lab/compose.yml run --rm --no-deps --entrypoint cat seed /out/api-key`.
