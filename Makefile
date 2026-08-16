SHELL := /bin/bash
.DEFAULT_GOAL := help

GO_DIR   := apps/runtime-go
PY_DIR   := apps/retriever-python
WEB_DIR  := apps/web
CODEGEN  := packages/contracts/codegen
GEN_DIRS := $(GO_DIR)/internal/contracts/gen $(PY_DIR)/src/models/gen $(WEB_DIR)/src/contracts/gen

COMPOSE      := docker compose -f infra/docker-compose.yml
COMPOSE_TEST := docker compose -f infra/docker-compose.test.yml

# The Python venvs are per-service and not committed; these are where the
# setup targets put them. PY_BIN is absolute so targets can cd into the
# package directory — which they must, because ruff and mypy read their
# configuration relative to the working directory, and running them from the
# repo root silently ignores pyproject.toml (and then lints generated code).
PY_VENV      := $(PY_DIR)/.venv
PY_BIN       := $(CURDIR)/$(PY_VENV)/bin
CODEGEN_VENV := $(CODEGEN)/.venv

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- setup ----

.PHONY: setup
setup: setup-codegen setup-go setup-python setup-web ## Install every toolchain and dependency

.PHONY: setup-codegen
setup-codegen: ## Install the pinned contract generators
	@set -euo pipefail; source $(CODEGEN)/versions.env; \
	go install $$GO_JSONSCHEMA_MODULE@$$GO_JSONSCHEMA_VERSION; \
	python3 -m venv $(CODEGEN_VENV); \
	$(CODEGEN_VENV)/bin/pip install --quiet "datamodel-code-generator==$$DATAMODEL_CODEGEN_VERSION"; \
	npm --prefix $(CODEGEN) ci --no-audit --no-fund

.PHONY: setup-go
setup-go: ## Download Go module dependencies
	cd $(GO_DIR) && go mod download

.PHONY: setup-python
setup-python: ## Create the retriever venv and install it editable
	python3 -m venv $(PY_VENV)
	$(PY_VENV)/bin/pip install --quiet -e "$(PY_DIR)[dev]"

.PHONY: setup-web
setup-web: ## Install web dependencies
	npm --prefix $(WEB_DIR) ci --no-audit --no-fund

# ------------------------------------------------------------ contracts ----

.PHONY: generate-schemas
generate-schemas: ## Regenerate contract types for Go, Python, and TypeScript
	$(CODEGEN)/generate.sh

.PHONY: check-schemas
check-schemas: generate-schemas ## Fail if generated contract code drifted from the schemas
	git diff --exit-code -- $(GEN_DIRS)

# ---------------------------------------------------------------- build ----
# `build` must stay green even while TODO(you) stubs are open: stubs compile,
# they just panic. That is what makes it a meaningful gate during a phase whose
# tests are red on purpose. See CLAUDE.md, "The two-job CI gate".

.PHONY: build
build: build-go build-python build-web ## Compile and type-check everything

.PHONY: build-go
build-go: ## Compile the Go runtime
	cd $(GO_DIR) && go build ./...

.PHONY: build-python
build-python: ## Type-check the retriever
	cd $(PY_DIR) && $(PY_BIN)/mypy src

.PHONY: build-web
build-web: ## Type-check and bundle the web app
	npm --prefix $(WEB_DIR) run build

.PHONY: lint
lint: ## Lint every service
	cd $(GO_DIR) && go vet ./...
	cd $(PY_DIR) && $(PY_BIN)/ruff check .
	npm --prefix $(WEB_DIR) run lint

# ----------------------------------------------------------------- test ----

.PHONY: test
test: test-go test-python test-web ## Run every test suite

.PHONY: test-go
test-go: ## Run Go tests under the race detector
	cd $(GO_DIR) && go test -race ./...

.PHONY: test-python
test-python: ## Run retriever tests with coverage
	cd $(PY_DIR) && $(PY_BIN)/pytest

.PHONY: test-web
test-web: ## Run web tests
	npm --prefix $(WEB_DIR) test

.PHONY: coverage
coverage: ## Report coverage for every service
	cd $(GO_DIR) && go test -cover ./...
	cd $(PY_DIR) && $(PY_BIN)/pytest
	npm --prefix $(WEB_DIR) run coverage

.PHONY: stubs
stubs: ## List open TODO(you) sites — the work that is Sami's, not Claude's
	@# The second filter requires the marker to BE the statement, not merely
	@# appear on the line. internal/index documents the contract in its comments
	@# and exercises it in test fixtures; a bare text search reported 12 sites
	@# when 4 were open, which makes the worklist useless.
	@grep -rn "TODO(you)" apps eval \
		--include=*.go --include=*.py --include=*.ts --include=*.tsx \
		2>/dev/null \
		| grep -E ':[0-9]+:[[:space:]]*(panic|raise|throw)' \
		|| echo "no open stubs"

# ---------------------------------------------------------------- plumb ----
# The claim-verification surface (PLAN.md section 5.7). Deterministic at P2.5:
# no model, no network, no credential.

PLUMB_BIN ?= $(GO_DIR)/bin/plumb

.PHONY: plumb
plumb: ## Build the plumb binary
	cd $(GO_DIR) && go build -o bin/plumb ./cmd/plumb

.PHONY: plumb-self
plumb-self: plumb ## Run plumb against this repository (informational)
	@# --workspace-siblings suits a multi-repo workspace and is deliberately not
	@# used by plumb-check, whose result must not depend on neighbouring folders.
	@$(PLUMB_BIN) verify --workspace-siblings . || true

.PHONY: plumb-check
plumb-check: plumb ## Fail if this repository's docs contradict it
	@# Deliberately NOT wired into CI yet, and not called a gate. PLAN.md 5.7
	@# lists two classes of finding that are inherent until the P6.5 claim layer
	@# lands, so this target cannot be green on this repository today. Wiring it
	@# needs a baseline or allowlist first.
	$(PLUMB_BIN) verify .

# ----------------------------------------------------------------- eval ----
# Placeholders until P2. They exist now so PLAN.md section 9's command list is
# real from the first commit rather than aspirational.

.PHONY: eval
eval: ## Full BIRD dev run — PAID, manual only (P2)
	@echo "not implemented until P2 — see PLAN.md section 6"; exit 1

.PHONY: eval-smoke
eval-smoke: ## Replayed eval subset, zero paid calls, identical to CI (P2)
	@echo "not implemented until P2 — see PLAN.md section 6.3"; exit 1

.PHONY: eval-report
eval-report: ## Metrics table and cost breakdown for the latest run (P2)
	@echo "not implemented until P2 — see PLAN.md section 6.2"; exit 1

# ----------------------------------------------------------------- data ----

.PHONY: toy-db
toy-db: ## Rebuild the committed toy SQLite fixture from its .sql source
	infra/scripts/build-toy-db.sh

.PHONY: fetch-bird
fetch-bird: ## Download the BIRD dev set (manual, license-gated, multi-GB)
	infra/scripts/fetch-bird.sh

# --------------------------------------------------------------- docker ----

.PHONY: up
up: ## Start the demo Postgres and both services
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop everything and remove volumes
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail service logs
	$(COMPOSE) logs -f

.PHONY: smoke
smoke: ## Run the compose smoke test
	$(COMPOSE_TEST) up --build --abort-on-container-exit --exit-code-from smoke
