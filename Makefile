.PHONY: up down build run test lint migrate seed eval verify-chain

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

run: build ## run nexusd in the foreground
	./bin/nexusd

test: ## unit + property tests (no external services required)
	go test ./...

lint: ## static analysis (golangci-lint 2.5.0, matches the source repo's pin)
	golangci-lint run ./...

# --- Data plane operations (stubs until their owning phase lands) ---

migrate: build ## apply SQL migrations incl. RLS policies, direct to postgres (bypasses pgbouncer on purpose)
	./bin/nexusd migrate

seed: build ## seed one tenant (TENANT=name, default acme); agent + skill seeding lands Phase 1/7
	./bin/nexusd seed --tenant=$(TENANT)

eval: ## run the eval corpus as the CI release gate (Phase 1 skeleton; hardens in Phase 9)
	go run ./evals/cmd/runner

verify-chain: ## verify the hash-chained audit log has no break or gap — lands Phase 5
	@echo "not yet implemented: internal/audit/verify.go lands Phase 5"
