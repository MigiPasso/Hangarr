# Every target is a docker / docker compose command: nothing runs on the host
# besides Docker (see CLAUDE.md). Compatible with GNU Make 3.81 (macOS).

COMPOSE      := docker compose -f compose.dev.yml
# Separate project: e2e teardown never touches the dev database or caches.
E2E_COMPOSE  := docker compose -p arrstats-e2e -f compose.dev.yml --profile e2e
ALL_PROFILES := --profile real --profile tools --profile e2e
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE        ?= arrstats
PLATFORMS    ?= linux/amd64,linux/arm64
BUILDER      ?= arrstats-builder
HTMX_VERSION ?= 2.0.11
COMMON_PASSWORDS_URL ?= https://raw.githubusercontent.com/danielmiessler/SecLists/master/Passwords/Common-Credentials/10k-most-common.txt

.DEFAULT_GOAL := help
.PHONY: help dev dev-real down logs test lint e2e image image-load reset-admin clean deps vendor-assets k8s-validate capture-fixtures sync

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

dev: ## Run app (hot reload) + mock upstreams on http://localhost:8080
	$(COMPOSE) up --build app mock

dev-real: ## Run app against the real instances from .env (DRY_RUN=true by default)
	@test -f .env || { echo ".env missing: copy .env.example and fill it" >&2; exit 1; }
	ARRSTATS_ENV_FILE=.env $(COMPOSE) --profile real up --build app-real

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
	$(E2E_COMPOSE) build app-e2e app-e2e-live mock
	@status=0; \
	$(E2E_COMPOSE) up -d --wait app-e2e app-e2e-live mock || status=$$?; \
	if [ $$status -eq 0 ]; then \
	  pw=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e | sed -n 's/^ Mot de passe : //p' | head -n 1); \
	  pwlive=$$($(E2E_COMPOSE) logs --no-log-prefix app-e2e-live | sed -n 's/^ Mot de passe : //p' | head -n 1); \
	  if [ -z "$$pw" ] || [ -z "$$pwlive" ]; then \
	    echo "initial admin password not found in app-e2e logs" >&2; status=1; \
	  else \
	    ADMIN_INITIAL_PASSWORD="$$pw" ADMIN_INITIAL_PASSWORD_LIVE="$$pwlive" $(E2E_COMPOSE) run --rm e2e || status=$$?; \
	  fi; \
	fi; \
	if [ $$status -ne 0 ]; then $(E2E_COMPOSE) logs --tail 50 app-e2e app-e2e-live mock; fi; \
	$(E2E_COMPOSE) down --remove-orphans; \
	exit $$status

image: ## Multi-arch runtime image (PUSH=1 to push to $(IMAGE))
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || docker buildx create --name $(BUILDER) --driver docker-container --config build/buildkitd.toml >/dev/null
	docker buildx build --builder $(BUILDER) --platform $(PLATFORMS) --target runtime \
	  --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) $(if $(PUSH),--push,) .

image-load: ## Runtime image for the local platform, loaded into docker
	docker build --target runtime --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):local .

reset-admin: ## Reset the admin password of the running dev app
	$(COMPOSE) exec app arrstats admin reset-password

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

k8s-validate: ## kustomize build + kubeconform (phase 6)
	@echo "k8s-validate arrives with the Kubernetes manifests in phase 6" >&2; exit 1

capture-fixtures: ## Record anonymized responses of the real instances (.env) into testdata/captured/
	@test -f .env || { echo ".env missing: copy .env.example and fill it" >&2; exit 1; }
	ARRSTATS_ENV_FILE=.env $(COMPOSE) --profile real run --rm capture

sync: ## Run one synchronization in the running dev app and print a summary
	$(COMPOSE) exec app arrstats sync --once
