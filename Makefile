# masterplayer — Docker helpers
# Requires: Docker + Docker Compose v2, and `make` (Git Bash / WSL / Linux).
# Local (non-Docker) targets at the bottom need Go and/or Node.

# The tailscaled socket is only mounted when device checking is on (MP_KNOWN_DEVICES set in .env).
KNOWN_DEVICES := $(shell grep -E '^MP_KNOWN_DEVICES=.+' .env 2>/dev/null)
COMPOSE ?= docker compose$(if $(KNOWN_DEVICES), -f docker-compose.yml -f docker-compose.tailscale.yml)
SERVICE ?= masterplayer

.DEFAULT_GOAL := help
.PHONY: help env-check build up dev down restart logs status ps shell clean logout-all \
        dirs test cover deploy smoke e2e \
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
	@echo "  make logout-all  Sign every browser out (sessions survive restarts otherwise)"
	@echo ""
	@echo "  make test     Run go vet + go test + the coverage gate in a throwaway container"
	@echo "  make smoke    Build the image, start it with a fixture library, check login/browse/streaming"
	@echo "  make e2e      Real uploads end to end (40 MiB resume, .zip and .7z archives)"
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

# Create the host folders as YOU. If Docker creates a missing bind-mount folder it is owned by
# root, and the app (which runs as PUID) could not write to it.
dirs: env-check ## Create the library, data and uploads folders as your user
	@for v in LIBRARY_HOST_PATH:./music DATA_HOST_PATH:./data UPLOADS_HOST_PATH:./uploads; do \
		name=$${v%%:*}; def=$${v#*:}; \
		val=$$(grep -E "^$$name=" .env | tail -1 | cut -d= -f2-); \
		mkdir -p "$${val:-$$def}"; \
	done

build: env-check dirs ## Build the image from scratch, ignoring the Docker layer cache
	$(COMPOSE) build --no-cache --pull

up: env-check dirs ## Build from scratch and replace the running container
	$(COMPOSE) build --no-cache --pull
	$(COMPOSE) up -d --force-recreate

dev: env-check dirs ## Cached rebuild + always replace the running container (fast iteration)
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

# Sessions are kept in DATA_HOST_PATH/sessions.json so restarts do not sign anyone out. Stop
# first, so a sign-in landing between the delete and the restart cannot write the file back.
logout-all: env-check ## Sign every browser out: stop, delete the sessions file, start again
	$(COMPOSE) stop $(SERVICE)
	@d=$$(grep -E '^DATA_HOST_PATH=' .env | tail -1 | cut -d= -f2-); \
		rm -f "$${d:-./data}/sessions.json" "$${d:-./data}/sessions.json.tmp" && \
		echo "Removed $${d:-./data}/sessions.json: everyone has to sign in again."
	$(COMPOSE) up -d $(SERVICE)

clean: down ## Stop containers and remove the project image
	-docker image rm masterplayer:latest

# Runs in the Dockerfile's `test` stage: go vet, go test and the coverage gate
# (COVER_MIN, default 100 like szmy's mandate — override with `make test COVER_MIN=90`).
# The module has no third-party dependencies, so there is no module cache volume to maintain.
COVER_MIN ?= 100

test: ## Run go vet + go test + coverage gate in a throwaway container
	docker build --pull --target test --build-arg COVER_MIN=$(COVER_MIN) .

smoke: ## Build the image, start it with a fixture library, check login/browse/streaming
	sh scripts/smoke.sh

e2e: ## Real uploads end to end: 40 MiB resume, .zip and .7z archives (builds the image)
	sh scripts/e2e.sh

deploy: ## git pull + test + up (run on the Ubuntu host)
	git pull
	$(MAKE) test
	$(MAKE) up

# ── Local development, no Docker ─────────────────────────────────────────

go-build: ## Build bin/masterplayer (needs Go 1.27+)
	go build -trimpath -o bin/masterplayer ./cmd/masterplayer

go-run: go-build ## Run locally against ./music and ./data
	MP_MUSIC_DIR=./music MP_DATA_DIR=./data ./bin/masterplayer

cover: ## Coverage gate on the host (needs Go)
	COVER_MIN=$(COVER_MIN) sh scripts/coverage.sh

web-test: ## Node tests for the browser logic (needs Node 20+)
	node --test tests/web/*.test.js
