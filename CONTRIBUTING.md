# Contributing

Thank you for helping with Hangarr. This guide covers the development workflow. The design is described in [docs/architecture.md](docs/architecture.md).

## Prerequisites

- Docker Engine and Docker Compose v2, version 2.24 or newer (`compose.yml` uses `env_file.required`).
- Docker Buildx, only to build multi-architecture images (`make image`).
- `make` (GNU Make 3.81 or newer) and `git`.

Nothing else is installed on the host. Go, the linters, Playwright and every other tool run in containers. Do not run `go`, `npm` or a browser on the host.

## Workflow

| Command | What it does |
|---|---|
| `make dev` | Runs the app with hot reload on http://localhost:8080, with the mock upstreams. `MEDIA=plex` (default: mock Plex and Tautulli), `MEDIA=jellyfin` (mock Jellyfin) or `MEDIA=none` (the media server is set in the interface). Use `DEV_PORT=8081 make dev` when 8080 is taken. |
| `make dev-real` | Runs the app against the real services listed in `.env` (copy `.env.example`). `DRY_RUN=true` by default. |
| `make test` | Unit and integration tests with the race detector (`go test -race -cover ./...`). |
| `make lint` | `go vet` and golangci-lint. |
| `make e2e` | Playwright scenarios against the runtime image, on a blank database, in the separate project `hangarr-e2e`. |
| `make verify` | gofmt, lint and tests, with a compact summary. Full logs go to `.verify/`. `make verify E2E=1` adds the end-to-end suite. |
| `make deps` | `go mod tidy`, in a container. |
| `make deploy-check` | Validates `compose.yml` and smoke-tests it against the mock, in the separate project `hangarr-deploy-check`. |
| `make k8s-check` | Validates `deploy/kubernetes/hangarr.yaml` against the Kubernetes schemas (kubeconform, in a container). |
| `make vulncheck` | Looks for known vulnerabilities in the Go code and its dependencies (govulncheck, in a container). |
| `make secret-scan` | Scans the whole git history for secrets (gitleaks with `.gitleaks.toml`, in a container). |
| `make capture-fixtures` | Records anonymized responses of the real services into `testdata/captured/`. Needs `.env`. |
| `make jellyfin-lab` | Starts a pinned Jellyfin with the test scenario on http://localhost:8096. `make jellyfin-lab-down` removes it. |
| `make clean` | Removes containers, volumes and the dev database. |
| `make help` | Lists the targets. |

Run `make verify` before opening a pull request. Run `make e2e` (or `make verify E2E=1`) when you change templates, CSS, JavaScript or anything else visible.

### Continuous integration

`.github/workflows/ci.yml` runs `make verify E2E=1`, `make deploy-check`, `make k8s-check`, `make vulncheck` and `make secret-scan` on pushes to `main` and on pull requests. It uses a read-only token and no secrets. `.github/workflows/release.yml` publishes the image to GHCR when a `v*` tag is pushed.

### Development sign-in

The dev instance (`make dev`) signs in with `admin` / `Hangarr-Dev-1`. This password exists for development only. It comes from `HANGARR_ADMIN_PASSWORD` in `compose.dev.yml` and applies only when the admin account does not exist yet, so it is used again after `make clean`. `make reset-admin` resets the password of the running dev app.

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
| Go | `GO_VERSION` in `Dockerfile`, and the `gotools` image in `compose.dev.yml` |
| golangci-lint, air | `Dockerfile` (build arguments) |
| Playwright | the `playwright` image in `compose.dev.yml`, and `@playwright/test` in `e2e/package.json` |
| htmx | `HTMX_VERSION` in `Makefile`; `make vendor-assets` downloads it |
| Common password list | `COMMON_PASSWORDS_URL` in `Makefile`; `make vendor-assets` downloads it |
| kubeconform, govulncheck, gitleaks | `KUBECONFORM_VERSION`, `GOVULNCHECK_VERSION`, `GITLEAKS_VERSION` in `Makefile` |
| GitHub Actions | pinned by commit SHA in `.github/workflows/`; Dependabot (`.github/dependabot.yml`) proposes Go module and action updates |

Keep the `Makefile` compatible with GNU Make 3.81, the version shipped with macOS.

## Pull requests

Before you open a pull request, check that:

- `make verify E2E=1` passes;
- every visible behaviour change has a scenario in `e2e/tests/`;
- new interface texts have a French translation in `internal/i18n/fr.go`;
- the documentation is updated when behaviour or configuration changes;
- the change contains no secret, no `.env` file and no real capture (`testdata/captured/`);
- a schema change is a new migration file.

`e2e/master.key` is a test-only key, committed on purpose. It is used only by the `app-e2e-config` service. It is not a secret.

## Reporting security issues

Please do not open a public issue for a suspected vulnerability; contact the maintainer privately.
