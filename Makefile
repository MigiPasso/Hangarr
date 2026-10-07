# Every target is a docker / docker compose command: nothing runs on the host
# besides Docker (see CLAUDE.md). Compatible with GNU Make 3.81 (macOS).

COMPOSE      := docker compose -f compose.dev.yml
# Separate project: e2e teardown never touches the dev database or caches.
E2E_COMPOSE  := MOCK_DEV_EXTRAS=false MOCK_REAL_POSTERS=false docker compose -p hangarr-e2e -f compose.dev.yml --profile e2e
# compose.yml as shipped, plus the mock and a smoke test (separate project).
DEPLOY_COMPOSE := docker compose -p hangarr-deploy-check -f compose.yml -f deploy/compose.check.yml
ALL_PROFILES := --profile real --profile tools --profile e2e
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE        ?= hangarr
PLATFORMS    ?= linux/amd64,linux/arm64
BUILDER      ?= hangarr-builder
HTMX_VERSION ?= 2.0.11
COMMON_PASSWORDS_URL ?= https://raw.githubusercontent.com/danielmiessler/SecLists/master/Passwords/Common-Credentials/10k-most-common.txt

.DEFAULT_GOAL := help
.PHONY: help dev dev-real down logs test lint e2e image image-load reset-admin clean deps vendor-assets deploy-check capture-fixtures sync

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

dev: ## Run app (hot reload) + mock upstreams on http://localhost:8080
	$(COMPOSE) up --build app mock

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
	$(E2E_COMPOSE) build app-e2e app-e2e-live app-e2e-config mock
	@status=0; \
	$(E2E_COMPOSE) up -d --wait app-e2e app-e2e-live app-e2e-config mock || status=$$?; \
	if [ $$status -eq 0 ]; then \
	  pw=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e | sed -n 's/^ Password : //p' | head -n 1); \
	  pwlive=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e-live | sed -n 's/^ Password : //p' | head -n 1); \
	  pwcfg=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e-config | sed -n 's/^ Password : //p' | head -n 1); \
	  if [ -z "$$pw" ] || [ -z "$$pwlive" ] || [ -z "$$pwcfg" ]; then \
	    echo "initial admin password not found in app-e2e logs" >&2; status=1; \
	  else \
	    ADMIN_INITIAL_PASSWORD="$$pw" ADMIN_INITIAL_PASSWORD_LIVE="$$pwlive" ADMIN_INITIAL_PASSWORD_CONFIG="$$pwcfg" \
	      $(E2E_COMPOSE) run --rm e2e || status=$$?; \
	  fi; \
	fi; \
	if [ $$status -ne 0 ]; then $(E2E_COMPOSE) logs --tail 50 app-e2e app-e2e-live app-e2e-config mock; fi; \
	$(E2E_COMPOSE) down --remove-orphans; \
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

deploy-check: ## Validate compose.yml and smoke-test it (as shipped) against the mock
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

capture-fixtures: ## Record anonymized responses of the real instances (.env) into testdata/captured/
	@test -f .env || { echo ".env missing: copy .env.example and fill it" >&2; exit 1; }
	HANGARR_ENV_FILE=.env $(COMPOSE) --profile real run --rm capture

sync: ## Run one synchronization in the running dev app and print a summary
	$(COMPOSE) exec app hangarr sync --once
