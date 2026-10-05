# masterplayer — Docker helpers
# Requires: Docker + Docker Compose v2, and `make` (Git Bash / WSL / Linux).
# Local (non-Docker) targets at the bottom need Go and/or Node.

COMPOSE ?= docker compose
SERVICE ?= masterplayer

.DEFAULT_GOAL := help
.PHONY: help env-check build up dev down restart logs status ps shell clean \
        test cover deploy smoke \
        go-build go-run web-test

help: ## Show this help
	@echo "masterplayer Docker targets"
	@echo ""
	@echo "  make up       Build from scratch (no cache) and start in background"
	@echo "  make dev      Fast cached rebuild + restart, for quick iteration"
	@echo "  make down     Stop containers"
	@echo "  make restart  Restart the container"
	@echo "  make logs     Follow logs"
	@echo "  make status   Show container status"
	@echo "  make shell    Shell into the running container"
	@echo "  make clean    Stop containers and remove the project image"
	@echo ""
	@echo "  make test     Run go vet + go test + the coverage gate in a throwaway container"
	@echo "  make smoke    Build the image, start it with a fixture library, check login/browse/streaming"
	@echo "  make deploy   git pull + test + up (run on the Ubuntu host)"
	@echo ""
	@echo "Without Docker: make go-build, make go-run, make web-test (Node tests for the browser logic)"
	@echo ""
	@echo "First time: cp .env.example .env, then set LIBRARY_HOST_PATH, PUID, PGID and"
	@echo "(optionally) MP_ADMIN_PASSWORD before 'make up'. Blank password = open access, no login."

env-check: ## Fail fast with a clear message if .env is missing
	@if [ ! -f .env ]; then \
		echo "No .env found. Run: cp .env.example .env, then fill it in."; \
		exit 1; \
	fi

build: env-check ## Build the image from scratch, ignoring the Docker layer cache
	$(COMPOSE) build --no-cache

up: env-check ## Build from scratch and replace the running container
	$(COMPOSE) build --no-cache
	$(COMPOSE) up -d --force-recreate

dev: env-check ## Cached rebuild + always replace the running container (fast iteration)
	$(COMPOSE) build
	$(COMPOSE) up -d --force-recreate

down: ## Stop and remove containers
	$(COMPOSE) down

restart: env-check ## Restart the container
	$(COMPOSE) restart $(SERVICE)

logs: ## Follow container logs
	$(COMPOSE) logs -f $(SERVICE)

status ps: ## Show container status
	$(COMPOSE) ps

shell: ## Open a shell in the running container
	$(COMPOSE) exec $(SERVICE) sh

clean: down ## Stop containers and remove the project image
	-docker image rm masterplayer:latest

# Runs in the Dockerfile's `test` stage: go vet, go test and the coverage gate
# (COVER_MIN, default 85 — override with `make test COVER_MIN=80`). The module has no
# third-party dependencies, so there is no module cache volume to maintain.
COVER_MIN ?= 85

test: ## Run go vet + go test + coverage gate in a throwaway container
	docker build --target test --build-arg COVER_MIN=$(COVER_MIN) .

smoke: ## Build the image, start it with a fixture library, check login/browse/streaming
	sh scripts/smoke.sh

deploy: ## git pull + test + up (run on the Ubuntu host)
	git pull
	$(MAKE) test
	$(MAKE) up

# ── Local development, no Docker ─────────────────────────────────────────

go-build: ## Build bin/masterplayer (needs Go 1.22+)
	go build -trimpath -o bin/masterplayer ./cmd/masterplayer

go-run: go-build ## Run locally against ./music and ./data
	MP_MUSIC_DIR=./music MP_DATA_DIR=./data ./bin/masterplayer

cover: ## Coverage gate on the host (needs Go)
	COVER_MIN=$(COVER_MIN) sh scripts/coverage.sh

web-test: ## Node tests for the browser logic (needs Node 20+)
	node --test tests/web/*.test.js
