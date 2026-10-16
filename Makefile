# Every target is a docker / docker compose command: nothing runs on the host
# besides Docker (see CONTRIBUTING.md). Compatible with GNU Make 3.81 (macOS).

COMPOSE      := docker compose -f compose.dev.yml
# Separate project: e2e teardown never touches the dev database or caches.
E2E_COMPOSE  := MOCK_DEV_EXTRAS=false MOCK_REAL_POSTERS=false docker compose -p hangarr-e2e -f compose.dev.yml --profile e2e
# compose.yml, plus the mock and a smoke test (separate project). The override deploy/compose.check.yml builds hangarr from the sources, not the published image.
DEPLOY_COMPOSE := docker compose -p hangarr-deploy-check -f compose.yml -f deploy/compose.check.yml
# Real Jellyfin with the test scenario (separate project, deploy/jellyfin-lab/compose.yml).
LAB_COMPOSE  := docker compose -f deploy/jellyfin-lab/compose.yml
ALL_PROFILES := --profile real --profile tools --profile e2e
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE        ?= hangarr
PLATFORMS    ?= linux/amd64,linux/arm64
BUILDER      ?= hangarr-builder
HTMX_VERSION ?= 2.0.11
KUBECONFORM_VERSION ?= v0.7.0
GOVULNCHECK_VERSION ?= v1.9.0
GITLEAKS_VERSION ?= v8.30.1
ALPINE_VERSION ?= 3.22
COMMON_PASSWORDS_URL ?= https://raw.githubusercontent.com/danielmiessler/SecLists/master/Passwords/Common-Credentials/10k-most-common.txt
# Media server of `make dev`: the variables read by compose.dev.yml (empty = unset for the app).
MEDIA        ?= plex
DEV_ENV_plex     := DEV_PLEX_URL=http://mock:32400 DEV_PLEX_TOKEN=mock-api-key DEV_TAUTULLI_URL=http://mock:8181 DEV_TAUTULLI_API_KEY=mock-api-key DEV_JELLYFIN_URL= DEV_JELLYFIN_API_KEY=
DEV_ENV_jellyfin := DEV_PLEX_URL= DEV_PLEX_TOKEN= DEV_TAUTULLI_URL= DEV_TAUTULLI_API_KEY= DEV_JELLYFIN_URL=http://mock:8096 DEV_JELLYFIN_API_KEY=mock-api-key
DEV_ENV_none     := DEV_PLEX_URL= DEV_PLEX_TOKEN= DEV_TAUTULLI_URL= DEV_TAUTULLI_API_KEY= DEV_JELLYFIN_URL= DEV_JELLYFIN_API_KEY=

VERIFY_DIR   := .verify
# Quiet progress: Compose writes "Container … Creating" lines to stderr, which would land in the logs.
VERIFY_COMPOSE := docker compose --progress quiet -f compose.dev.yml

.DEFAULT_GOAL := help
.PHONY: help dev dev-real down logs test lint e2e verify go-version-check image image-load reset-admin clean deps vendor-assets deploy-check capture-fixtures jellyfin-lab jellyfin-lab-capture jellyfin-lab-down sync icons k8s-check vulncheck secret-scan

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

dev: ## Run app (hot reload) + mock on http://localhost:8080. MEDIA=plex (default: Plex + Tautulli of the mock), jellyfin (Jellyfin of the mock) or none (server set in the UI)
	$(if $(filter plex jellyfin none,$(MEDIA)),,$(error MEDIA must be plex, jellyfin or none, not "$(MEDIA)"))
	$(DEV_ENV_$(MEDIA)) $(COMPOSE) up --build app mock

dev-real: ## Run app against the real instances from .env (DRY_RUN=true by default)
	@test -f .env || { echo ".env missing: copy .env.example and fill it" >&2; exit 1; }
	HANGARR_ENV_FILE=.env $(COMPOSE) --profile real up --build app-real

down: ## Stop the dev environment (keeps volumes)
	$(COMPOSE) $(ALL_PROFILES) down --remove-orphans

logs: ## Follow dev app logs
	$(COMPOSE) logs -f app

test: ## Unit + integration tests (go test -race -cover)
	$(COMPOSE) --profile tools build test
	$(COMPOSE) --profile tools run --rm test

lint: ## go vet + golangci-lint
	$(COMPOSE) --profile tools build lint
	$(COMPOSE) --profile tools run --rm lint

e2e: ## Playwright against the runtime image on a blank database
	@$(E2E_COMPOSE) down --remove-orphans >/dev/null 2>&1 || true
	$(E2E_COMPOSE) build app-e2e app-e2e-live app-e2e-config app-e2e-jellyfin mock
	@status=0; \
	$(E2E_COMPOSE) up -d --wait app-e2e app-e2e-live app-e2e-config app-e2e-jellyfin mock || status=$$?; \
	if [ $$status -eq 0 ]; then \
	  pw=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e | sed -n 's/^ Password : //p' | head -n 1); \
	  pwcfg=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e-config | sed -n 's/^ Password : //p' | head -n 1); \
	  if [ -z "$$pw" ] || [ -z "$$pwcfg" ]; then \
	    echo "initial admin password not found in app-e2e logs" >&2; status=1; \
	  elif $(E2E_COMPOSE) logs --no-log-prefix app-e2e-live | grep -q "INITIAL ADMIN"; then \
	    echo "app-e2e-live printed a generated password despite HANGARR_ADMIN_PASSWORD" >&2; status=1; \
	  else \
	    ADMIN_INITIAL_PASSWORD="$$pw" ADMIN_INITIAL_PASSWORD_CONFIG="$$pwcfg" \
	      $(E2E_COMPOSE) run --rm e2e || status=$$?; \
	  fi; \
	fi; \
	if [ $$status -ne 0 ]; then $(E2E_COMPOSE) logs --tail 50 app-e2e app-e2e-live app-e2e-config app-e2e-jellyfin mock; fi; \
	$(E2E_COMPOSE) down --remove-orphans; \
	exit $$status

go-version-check: ## The Go version of go.mod (toolchain), the Dockerfile and the Compose images must match
	@want=$$(sed -n 's/^ARG GO_VERSION=//p' Dockerfile); \
	tc=$$(sed -n 's/^toolchain go//p' go.mod); \
	bad=$$(grep -h -o 'golang:[0-9][0-9.]*' compose.dev.yml deploy/jellyfin-lab/compose.yml | sed 's/^golang://' | grep -v -x -F "$$want" | sort -u); \
	if [ -z "$$want" ] || [ "$$tc" != "$$want" ] || [ -n "$$bad" ]; then \
	  echo "Go versions differ: Dockerfile GO_VERSION=$$want, go.mod toolchain=go$$tc, Compose images: $${bad:-ok}" >&2; exit 1; \
	fi; echo "go version: OK ($$want)"

verify: ## go versions + gofmt + lint + test (+ e2e with E2E=1): compact summary, full logs in .verify/
	@mkdir -p $(VERIFY_DIR); rm -f $(VERIFY_DIR)/*.log; status=0; \
	$(MAKE) --no-print-directory go-version-check || status=1; \
	$(VERIFY_COMPOSE) --profile tools run --rm -T gotools gofmt -l . > $(VERIFY_DIR)/gofmt.log 2>&1; rc=$$?; \
	if [ $$rc -eq 0 ] && [ ! -s $(VERIFY_DIR)/gofmt.log ]; then \
	  echo "gofmt: OK"; \
	else \
	  echo "gofmt: FAIL"; cat $(VERIFY_DIR)/gofmt.log; status=1; \
	fi; \
	$(VERIFY_COMPOSE) --profile tools build -q lint > $(VERIFY_DIR)/lint.log 2>&1; rc=$$?; \
	if [ $$rc -eq 0 ]; then $(VERIFY_COMPOSE) --profile tools run --rm -T lint >> $(VERIFY_DIR)/lint.log 2>&1; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then \
	  echo "lint: OK"; \
	else \
	  echo "lint: FAIL"; head -n 200 $(VERIFY_DIR)/lint.log; status=1; \
	fi; \
	$(VERIFY_COMPOSE) --profile tools build -q test > $(VERIFY_DIR)/test.log 2>&1; rc=$$?; \
	if [ $$rc -eq 0 ]; then $(VERIFY_COMPOSE) --profile tools run --rm -T test >> $(VERIFY_DIR)/test.log 2>&1; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then \
	  echo "test: OK ($$(grep -c -E '(^|[[:space:]])ok[[:space:]]' $(VERIFY_DIR)/test.log) packages)"; \
	else \
	  echo "test: FAIL"; grep -v -E '^(ok|\?)[[:space:]]|^[[:space:]]+[^[:space:]]+[[:space:]]+coverage: 0\.0% of statements$$' $(VERIFY_DIR)/test.log | head -n 200; status=1; \
	fi; \
	if [ -n "$(E2E)" ]; then \
	  if [ $$status -ne 0 ]; then \
	    echo "e2e: skipped (earlier failure)"; \
	  else \
	    $(MAKE) --no-print-directory e2e > $(VERIFY_DIR)/e2e.log 2>&1; rc=$$?; \
	    if [ $$rc -eq 0 ]; then \
	      grep -E '^ +[0-9]+ (passed|failed|flaky|skipped|did not run)' $(VERIFY_DIR)/e2e.log; \
	      echo "e2e: OK"; \
	    else \
	      echo "e2e: FAIL"; \
	      grep -E '^ +[0-9]+ (passed|failed|flaky|skipped|did not run)' $(VERIFY_DIR)/e2e.log; \
	      if grep -q -E '^  [0-9]+\) ' $(VERIFY_DIR)/e2e.log; then \
	        sed -n '/^  [0-9][0-9]*) /,$$p' $(VERIFY_DIR)/e2e.log | head -n 300; \
	      else \
	        tail -n 60 $(VERIFY_DIR)/e2e.log; \
	      fi; \
	      status=1; \
	    fi; \
	  fi; \
	fi; \
	if [ $$status -eq 0 ]; then echo "verify: OK"; \
	else echo "verify: FAIL (full logs: $(VERIFY_DIR)/*.log)"; fi; \
	exit $$status

image: ## Multi-arch runtime image (PUSH=1 to push to $(IMAGE))
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || docker buildx create --name $(BUILDER) --driver docker-container --config build/buildkitd.toml >/dev/null
	docker buildx build --builder $(BUILDER) --platform $(PLATFORMS) --target runtime \
	  --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) $(if $(PUSH),--push,) .

image-load: ## Runtime image for the local platform, loaded into docker
	docker build --target runtime --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):local .

reset-admin: ## Reset the admin password of the running dev app
	$(COMPOSE) exec app hangarr admin reset-password

clean: ## Remove containers, volumes and the dev database
	$(COMPOSE) $(ALL_PROFILES) down -v --remove-orphans
	$(E2E_COMPOSE) down -v --remove-orphans

deps: ## go mod tidy (in a container)
	$(COMPOSE) --profile tools run --rm gotools go mod tidy

vendor-assets: ## Download htmx and the common password list (in a container)
	$(COMPOSE) --profile tools run --rm fetch -fsSL -o internal/web/static/htmx.min.js \
	  https://unpkg.com/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js
	$(COMPOSE) --profile tools run --rm --entrypoint sh fetch -c '\
	  set -e; tmp=$$(mktemp); curl -fsSL -o "$$tmp" "$(COMMON_PASSWORDS_URL)"; \
	  { echo "# Source: SecLists (MIT), $(COMMON_PASSWORDS_URL)"; cat "$$tmp"; } > internal/auth/common-passwords.txt'

deploy-check: ## Validate compose.yml and smoke-test it, with an image built from the sources, against the mock
	docker compose -f compose.yml config -q
	@$(DEPLOY_COMPOSE) down -v --remove-orphans >/dev/null 2>&1 || true
	$(DEPLOY_COMPOSE) build hangarr mock
	@status=0; \
	$(DEPLOY_COMPOSE) up -d --wait hangarr mock || status=$$?; \
	if [ $$status -eq 0 ]; then $(DEPLOY_COMPOSE) run --rm check || status=$$?; fi; \
	if [ $$status -eq 0 ]; then \
	  logs=$$($(DEPLOY_COMPOSE) logs --no-log-prefix hangarr); \
	  echo "$$logs" | grep -q "INITIAL ADMIN ACCOUNT CREATED" || { echo "FAIL: no initial admin banner" >&2; status=1; }; \
	  echo "$$logs" | grep -q "generated a master key" || { echo "FAIL: master key not generated in /data" >&2; status=1; }; \
	fi; \
	if [ $$status -eq 0 ]; then \
	  $(DEPLOY_COMPOSE) exec -T hangarr /hangarr backup >/dev/null || { echo "FAIL: backup" >&2; status=1; }; \
	fi; \
	if [ $$status -eq 0 ]; then echo "deploy check: OK"; else $(DEPLOY_COMPOSE) logs --tail 40 hangarr; fi; \
	$(DEPLOY_COMPOSE) down -v --remove-orphans; \
	exit $$status

k8s-check: ## Validate deploy/kubernetes/hangarr.yaml against the Kubernetes schemas (kubeconform, in a container)
	docker run --rm -v "$(CURDIR)/deploy/kubernetes:/k:ro" ghcr.io/yannh/kubeconform:$(KUBECONFORM_VERSION) -strict -summary /k/hangarr.yaml

vulncheck: ## Known vulnerabilities in the Go code and its dependencies (govulncheck, in a container)
	$(COMPOSE) --profile tools run --rm -T gotools go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

secret-scan: ## Secrets in the whole git history (gitleaks with .gitleaks.toml, in a container)
	docker run --rm -v "$(CURDIR):/repo:ro" ghcr.io/gitleaks/gitleaks:$(GITLEAKS_VERSION) git /repo -c /repo/.gitleaks.toml --log-opts=--all --redact --no-banner

capture-fixtures: ## Record anonymized responses of the real instances (.env) into testdata/captured/
	@test -f .env || { echo ".env missing: copy .env.example and fill it" >&2; exit 1; }
	HANGARR_ENV_FILE=.env HOST_UID=$(shell id -u) HOST_GID=$(shell id -g) $(COMPOSE) --profile real run --rm capture

jellyfin-lab: ## Real Jellyfin (pinned) with the test scenario on http://localhost:8096
	$(LAB_COMPOSE) up -d --wait jellyfin
	$(LAB_COMPOSE) run --rm seed

jellyfin-lab-capture: ## Record anonymized responses of the Jellyfin lab into testdata/captured/jellyfin/
	HOST_UID=$(shell id -u) HOST_GID=$(shell id -g) $(LAB_COMPOSE) --profile capture run --rm capture

jellyfin-lab-down: ## Remove the Jellyfin lab (containers and volumes)
	$(LAB_COMPOSE) --profile capture down -v --remove-orphans

sync: ## Run one synchronization in the running dev app and print a summary
	$(COMPOSE) exec app hangarr sync --once

icons: ## Render the PNG icons (tab fallback, apple-touch-icon) from static/favicon.svg (in a container)
	docker run --rm -v "$(CURDIR)/internal/web/static:/s" -w /s alpine:$(ALPINE_VERSION) sh -c '\
	  set -e; apk add --no-cache -q rsvg-convert; \
	  rsvg-convert -w 32 -h 32 -o favicon-32.png favicon.svg; \
	  rsvg-convert --page-width 180 --page-height 180 --left 22 --top 22 -w 136 -h 136 \
	    --background-color "#eef0f2" -o apple-touch-icon.png favicon.svg; \
	  chown $(shell id -u):$(shell id -g) favicon-32.png apple-touch-icon.png'
