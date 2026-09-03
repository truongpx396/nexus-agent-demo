.PHONY: up down build run signerd test lint migrate seed eval eval-baseline verify-chain erase dashboard go-live web-build

TENANT ?= acme

# --- Infrastructure (Postgres + PgBouncer + Redis) ---

up: ## start postgres, pgbouncer, redis
	docker compose -f deploy/docker-compose.yml up -d
	@echo "postgres:5433  pgbouncer:6432 (transaction pooling)  redis:6380"

down: ## stop and remove infrastructure containers (volumes kept)
	docker compose -f deploy/docker-compose.yml down

# --- Go build / test / lint ---

build: ## build all three binaries into ./bin
	go build -o bin/nexusd  ./cmd/nexusd
	go build -o bin/nexusctl ./cmd/nexusctl
	go build -o bin/signerd ./cmd/signerd

run: build ## run signerd in the background + nexusd in the foreground (Ctrl-C stops both)
	./bin/signerd & echo $$! > .dev/signerd.pid
	@trap 'kill `cat .dev/signerd.pid` 2>/dev/null; rm -f .dev/signerd.pid' EXIT INT TERM; \
	./bin/nexusd

signerd: build ## run signerd alone in the foreground — nexusd's Kernel.Receipts (README task 5.2) needs it reachable at NEXUS_SIGNERD_SOCKET (default .dev/signerd.sock) before any event can append
	./bin/signerd

test: ## unit + property tests (no external services required)
	go test ./...

lint: ## static analysis (golangci-lint 2.5.0, matches the source repo's pin)
	golangci-lint run ./...

# --- Data plane operations (stubs until their owning phase lands) ---

migrate: build ## apply SQL migrations incl. RLS policies, direct to postgres (bypasses pgbouncer on purpose)
	./bin/nexusd migrate

seed: build ## seed one tenant (TENANT=name, default acme) + its default price book; agent + skill seeding lands Phase 1/7
	./bin/nexusd seed --tenant=$(TENANT)

eval: ## run the eval corpus as the CI release gate: k trials, Wilson intervals, class policies, held-out gap, efficiency gating, baseline regression check (Phase 10)
	go run ./evals/cmd/runner

eval-baseline: ## regenerate evals/testdata/baseline.json from the current corpus's own run — commit the diff once the corpus is deliberately changed
	go run ./evals/cmd/runner -update-baseline

verify-chain: build ## verify the hash-chained audit log has no break or gap (README task 5.3)
	./bin/nexusd verify-chain

dashboard: build ## print the golden-signal dashboard per tenant (README task 10.12)
	./bin/nexusd dashboard

go-live: build ## run the go-live checklist against a live deployment (README task 10.13, docs/go-live.md)
	./bin/nexusd go-live

# --- Web ---

web-build: ## build the React web app (README task 11.7)
	cd web && npm ci && npm run build
