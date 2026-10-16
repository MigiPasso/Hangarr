# Contributing

Hangarr is developed by its maintainer. The repository does not accept issues, and pull requests are limited to collaborators. This guide describes the development workflow and conventions, for collaborators and for anyone working on a fork. The design is described in [docs/architecture.md](docs/architecture.md).

## Prerequisites

- Docker Engine and Docker Compose v2, version 2.24 or newer (`compose.yml` uses `env_file.required`).
- Docker Buildx, only to build multi-architecture images (`make image`).
- `make` (GNU Make 3.81 or newer) and `git`.

Nothing else is installed on the host. Go, the linters, Playwright and every other tool run in containers. Do not run `go`, `npm` or a browser on the host.

## Workflow

| Command | What it does |
|---|---|
| `make dev` | Runs the app with hot reload on http://localhost:8080, with the mock upstreams. `MEDIA=plex` (default: mock Plex and Tautulli), `MEDIA=jellyfin` (mock Jellyfin) or `MEDIA=none` (the media server is set in the interface). Use `DEV_PORT=8081 make dev` when 8080 is taken. |
| `make dev-real` | Runs the app against the real services listed in `.env` (copy `.env.example`). `DRY_RUN=true` by default. The admin password is generated and printed once in the logs (`make logs` shows the dev app only: read the output of `make dev-real`), unless `.env` sets `HANGARR_ADMIN_PASSWORD`. |
| `make test` | Unit and integration tests with the race detector (`go test -race -cover ./...`). |
| `make lint` | `go vet` and golangci-lint. |
| `make e2e` | Playwright scenarios against the runtime image, on a blank database, in the separate project `hangarr-e2e`. |
| `make verify` | gofmt, lint and tests, with a compact summary. Full logs go to `.verify/`. `make verify E2E=1` adds the end-to-end suite. |
| `make deps` | `go mod tidy`, in a container. |
| `make deploy-check` | Validates `compose.yml` and smoke-tests it against the mock, in the separate project `hangarr-deploy-check`. |
| `make k8s-check` | Validates `deploy/kubernetes/hangarr.yaml` against the Kubernetes schemas (kubeconform, in a container). |
| `make vulncheck` | Looks for known vulnerabilities in the Go code and its dependencies (govulncheck, in a container). |
| `make scan` | Builds the runtime image and fails on a fixable HIGH or CRITICAL vulnerability (Trivy, in a container). |
| `make fuzz` | Runs every fuzz test (parsers of external input) for `FUZZTIME` each, 10 seconds by default (in a container). A failing input is saved under `testdata/fuzz/` of the package: commit it with the fix, it then runs with `make test`. |
| `make secret-scan` | Scans the whole git history for secrets (gitleaks with `.gitleaks.toml`, in a container). |
| `make capture-fixtures` | Records anonymized responses of the real services into `testdata/captured/`. Needs `.env`. |
| `make jellyfin-lab` | Starts a pinned Jellyfin with the test scenario on http://localhost:8096. `make jellyfin-lab-down` removes it. |
| `make clean` | Removes containers, volumes and the dev database. |
| `make help` | Lists the targets. |

Run `make verify` before opening a pull request. Run `make e2e` (or `make verify E2E=1`) when you change templates, CSS, JavaScript or anything else visible.

### Continuous integration

`.github/workflows/ci.yml` runs `make verify` and `make e2e` (two parallel jobs, together equivalent to `make verify E2E=1`), `make deploy-check`, `make k8s-check`, `make vulncheck`, `make scan`, `make fuzz` and `make secret-scan` on pull requests, which must pass on a branch up to date with `main` before a merge. It uses a read-only token and no secrets. `.github/workflows/release.yml` publishes the image to GHCR when a `v*` tag is pushed, only after `make verify`, `make e2e` and `make scan` pass on the tagged commit; the image is signed with cosign (keyless) and carries BuildKit SBOM and provenance attestations, which are not signed separately.

### Development sign-in

The dev instance (`make dev`) signs in with `admin` / `Hangarr-Dev-1`. This password exists for development only, so both dev instances listen on `127.0.0.1` only: they cannot be reached from the LAN. It comes from `HANGARR_ADMIN_PASSWORD` in `compose.dev.yml` and applies only when the admin account does not exist yet, so it is used again after `make clean`. `make reset-admin` resets the password of the running dev app.

`compose.dev.yml` also sets a fixed master key for the dev environment. It is public and must never be used anywhere else.

## Mock upstreams and fixtures

- `internal/mockarr` (served by `cmd/mockarr`) impersonates Sonarr (port 8989), Radarr (7878), Plex (32400), Jellyfin (8096) and Tautulli (8181). It answers from the JSON files in `testdata/`.
- Write calls (`DELETE`, `PUT`, `POST`) are recorded and listed on `GET /_calls`. `POST /_reset` clears them. The integration and end-to-end tests check deletions this way.
- `testdata/README.md` describes the synthetic scenario. The tests depend on its exact values: change it with care.
- `testdata/dev/`, `testdata/posters.json` and `testdata/fanarts.json` are used by `make dev` only.
- `testdata/captured/` holds real, anonymized responses. It is git-ignored and must never be committed. The tests only check that these files decode.

## Conventions

- **Language.** Code, identifiers, comments, log messages and the command line are in English. Keep comments rare, as in the surrounding code.
- **Interface text.** In templates, use `{{t "English text"}}` and `{{tn n "%d item" "%d items"}}`. In Go, use `l.T(...)` and `l.N(...)`. The French translation goes in `internal/i18n/fr.go`. `TestFrenchCatalogComplete` (`internal/web/i18n_test.go`) fails when a key has no translation. Identical English texts with different meanings need a `|context` suffix, which is not displayed.
- **Content Security Policy.** Templates must not contain an inline `<script>`, a `style="..."` attribute or an `on...=` handler attribute. For progress bars, use `<progress>`, `<meter>` or SVG.
- **CSS.** No hard-coded colors: use a variable defined in `app.css`. Optional styles depend on these variables. Put tables in `<div class="table-wrap">`. Numeric fields use the `.stepper` control, not native number arrows.
- **Authentication and CSRF.** Every authenticated POST, PUT or DELETE route is registered through `authed()`, which checks the CSRF token.
- **Outbound requests.** Calls to Sonarr, Radarr, Plex, Jellyfin and Tautulli go through `internal/clients/httpx`. Never use `http.DefaultClient` for an authenticated request.
- **SQL.** SQL lives only in `internal/store`, and every query is parameterized.
- **Migrations.** A new schema change is a new file `internal/store/migrations/NNNN_name.sql`. A migration that has been merged is never edited, not even its comments.
- **Pages.** A page is `internal/web/templates/pages/<name>.html`, declared in `pages` in `internal/web/views.go`.

### Pinned versions

Keep each pinned version identical in every place that lists it.

| Tool | Where it is pinned |
|---|---|
| Go | the `golang` image (tag and digest) in `Dockerfile`, `compose.dev.yml` and `deploy/jellyfin-lab/compose.yml`, and `toolchain` in `go.mod`; `make go-version-check` (part of `make verify`) fails when they differ |
| Base images (golangci-lint, distroless, Playwright, curl, alpine, Jellyfin) | pinned by tag **and** `@sha256:` digest in `Dockerfile` and the Compose files; Dependabot proposes the updates |
| air | `AIR_VERSION` in `Dockerfile` |
| Playwright | the `playwright` image in `compose.dev.yml`, and `@playwright/test` in `e2e/package.json` |
| htmx | `HTMX_VERSION` and `HTMX_SHA256` in `Makefile`; `make vendor-assets` downloads it and checks the checksum |
| Common password list | `COMMON_PASSWORDS_URL` (a SecLists commit) and `COMMON_PASSWORDS_SHA256` in `Makefile`; `make vendor-assets` downloads and checks it |
| kubeconform, govulncheck, gitleaks, Trivy | `KUBECONFORM_VERSION`, `GOVULNCHECK_VERSION`, `GITLEAKS_VERSION`, `TRIVY_IMAGE` in `Makefile` |
| GitHub Actions | pinned by commit SHA in `.github/workflows/`; Dependabot (`.github/dependabot.yml`) proposes Go module, action and image updates |

Keep the `Makefile` compatible with GNU Make 3.81, the version shipped with macOS.

## Pull request checklist

Before you open a pull request, check that:

- `make verify E2E=1` passes;
- every visible behaviour change has a scenario in `e2e/tests/`;
- new interface texts have a French translation in `internal/i18n/fr.go`;
- the documentation is updated when behaviour or configuration changes;
- the change contains no secret, no `.env` file and no real capture (`testdata/captured/`);
- a schema change is a new migration file.

`e2e/master.key` is a test-only key, committed on purpose. It is used only by the `app-e2e-config` service. It is not a secret.

Code merged into the repository is released under the project's [MIT License](LICENSE).

## Reporting security issues

Report a suspected vulnerability privately through GitHub: **Security** tab › **Report a vulnerability** (private vulnerability reporting). Do not disclose it publicly before a fix is available.
