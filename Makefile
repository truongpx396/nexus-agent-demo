.PHONY: up down build run signerd token test lint migrate seed eval eval-baseline verify-chain erase dashboard go-live web-build docker-build docker-up docker-down ollama-pull llm-up langfuse-up langfuse-lite-up llm-down agentic-up agentic-down observability-up observability-down tempo-up profiling-up pprof-cpu pprof-heap pprof-goroutine

TENANT ?= acme
# nexusd's own pprof listener (obs.StartPprofServer, NEXUS_PPROF_ADDR) --
# `run`/`signerd` below set this AND NEXUS_SIGNERD_PPROF_ADDR unconditionally
# (same "zero extra steps for the common path" call already made for
# NEXUS_LOG_FILE), so override PPROF_ADDR=127.0.0.1:6061 to point the
# pprof-* targets at signerd instead.
PPROF_ADDR ?= 127.0.0.1:6060

# --- Infrastructure (Postgres + PgBouncer + Redis) ---

up: ## start postgres, pgbouncer, redis
	docker compose -f deploy/docker-compose.yml up -d
	@echo "postgres:5433  pgbouncer:6432 (transaction pooling)  redis:6380"

down: ## stop and remove infrastructure containers (volumes kept)
	docker compose -f deploy/docker-compose.yml down

docker-build: ## build the nexusd and signerd images (README task 13.3) -- THROUGH compose's own `build:` stanza, not a bare `docker build`: a bare `docker build -t nexus-agent-demo/nexusd:latest .` tags a DIFFERENT image name than compose's own auto-generated one (`nexus-agent-demo-nexusd`), which `docker-up` below actually runs -- confirmed this silently left `docker-up` serving a days-old image after a source change, since `docker compose up` never rebuilds an image that already exists unless asked. `docker compose build` is the fix, not a workaround: it builds/tags the EXACT image `up` below will use.
	docker compose -f deploy/docker-compose.yml --profile app build

docker-up: docker-build ## start nexusd + signerd (built images) against the existing infra services
	docker compose -f deploy/docker-compose.yml --profile app up -d

docker-down: ## stop nexusd + signerd (the --profile app containers) -- `make down` alone does NOT stop these (confirmed: plain `docker compose down` ignores profile-gated services that are still running); infra (postgres/pgbouncer/redis) stays up
	docker compose -f deploy/docker-compose.yml --profile app down

# --- Go build / test / lint ---

build: ## build all three binaries into ./bin
	go build -o bin/nexusd  ./cmd/nexusd
	go build -o bin/nexusctl ./cmd/nexusctl
	go build -o bin/signerd ./cmd/signerd

run: build ## run signerd in the background + nexusd in the foreground (Ctrl-C stops both) -- --dev keeps the zero-setup path (auto KEK/AuthN key, fake provider); a real deployment omits it (README task 13.1/13.11). Both also tee structured logs to .dev/*.log (NEXUS_LOG_FILE, internal/obs.InitLogger) so `make observability-up`'s Promtail has something to tail even though neither runs in Docker by default. Both also start their own loopback-only pprof listener (NEXUS_PPROF_ADDR/NEXUS_SIGNERD_PPROF_ADDR, obs.StartPprofServer) so `make pprof-cpu`/`pprof-heap`/`pprof-goroutine` work with zero setup -- unset the var (or override to empty) to go back to no pprof listener, same as NEXUS_LOG_FILE's own opt-out.
	NEXUS_LOG_FILE=.dev/signerd.log NEXUS_SIGNERD_PPROF_ADDR=127.0.0.1:6061 ./bin/signerd & echo $$! > .dev/signerd.pid
	@trap 'kill `cat .dev/signerd.pid` 2>/dev/null; rm -f .dev/signerd.pid' EXIT INT TERM; \
	NEXUS_LOG_FILE=.dev/nexusd.log NEXUS_PPROF_ADDR=127.0.0.1:6060 ./bin/nexusd --dev

signerd: build ## run signerd alone in the foreground — nexusd's Kernel.Receipts (README task 5.2) needs it reachable at NEXUS_SIGNERD_SOCKET (default .dev/signerd.sock) before any event can append
	NEXUS_LOG_FILE=.dev/signerd.log NEXUS_SIGNERD_PPROF_ADDR=127.0.0.1:6061 ./bin/signerd

token: build ## mint a dev bearer token (TENANT=name, default acme) for curl/nexusctl/the web app -- nexusctl run "..." NEXUS_TOKEN=$$(make -s token)
	./bin/nexusd --dev token --tenant=$(TENANT)

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

# --- Local model + observability (docs/local-llm.md) ---
# A separate compose file (deploy/docker-compose.local-llm.yml), not more
# services in the one above: nothing here references, or is referenced by,
# postgres/pgbouncer/redis/signerd/nexusd, so a plain `down` on this file
# can never touch them — no need for the explicit-service-name workaround a
# shared file would require. Ollama itself is never a target here — it runs
# natively on the Mac host (Metal acceleration), started separately
# (`ollama serve`, or the menubar app).

ollama-pull: ## pull the local dev model into a natively-running Ollama (requires `ollama serve` already up)
	ollama pull qwen2.5:3b

llm-up: ## start the LiteLLM proxy in front of host Ollama (NEXUS_PROVIDER=litellm's target)
	docker compose -f deploy/docker-compose.local-llm.yml --profile llm up -d
	@echo "litellm: http://localhost:4100  (model: qwen2.5-local -> ollama_chat/qwen2.5:3b)"

langfuse-up: ## start a self-hosted Langfuse stack (web, worker, clickhouse, minio, redis, postgres) for agent tracing -- pair with NEXUS_OTLP_ENDPOINT (docs/local-llm.md)
	docker compose -f deploy/docker-compose.local-llm.yml --profile langfuse up -d
	@echo "langfuse: http://localhost:3001  (dev@nexus.local / nexus-dev-password)"

langfuse-lite-up: ## start the lightweight Langfuse v2 stack (web + postgres only, no worker/clickhouse/minio/redis) -- pair with NEXUS_LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY (docs/local-llm.md), NOT NEXUS_OTLP_ENDPOINT; only bring up one of `langfuse-up`/`langfuse-lite-up` at a time (both use host port 3001)
	docker compose -f deploy/docker-compose.local-llm.yml --profile langfuse-lite up -d
	@echo "langfuse (lite): http://localhost:3001  (dev@nexus.local / nexus-dev-password)"

llm-down: ## stop the litellm + langfuse (+ langfuse-lite) containers (postgres/pgbouncer/redis from `make up` are untouched — separate compose file)
	docker compose -f deploy/docker-compose.local-llm.yml --profile llm --profile langfuse --profile langfuse-lite down

# --- Agentic capabilities: Crawl4AI + OpenSandbox (docs/agentic-capabilities.md) ---
# A third, separate compose file — same reasoning as the local-llm block
# above: nothing here references postgres/pgbouncer/redis/signerd/nexusd, so
# `make down`/`make llm-down` can never touch it.

agentic-up: ## start Crawl4AI (platform/web_crawl) + an OpenSandbox server (NEXUS_SANDBOX=opensandbox)
	docker compose -f deploy/docker-compose.agentic.yml --profile agentic up -d
	@echo "crawl4ai: http://localhost:11235  (NEXUS_CRAWL4AI_URL, NEXUS_CRAWL4AI_API_TOKEN=nexus-dev-crawl4ai-token)"
	@echo "opensandbox: http://localhost:8090  (NEXUS_SANDBOX=opensandbox, NEXUS_OPENSANDBOX_URL, NEXUS_OPENSANDBOX_API_KEY=nexus-dev-opensandbox-key)"

agentic-down: ## stop the crawl4ai + opensandbox-server containers
	docker compose -f deploy/docker-compose.agentic.yml --profile agentic down

# --- Observability: Prometheus + Loki + Grafana (docs/observability.md) ---
# A fifth, separate compose file — same reasoning as the llm/agentic blocks
# above: nothing here references postgres/pgbouncer/redis/signerd/nexusd, so
# `make down`/`make llm-down`/`make agentic-down` can never touch it.
# Prometheus scrapes nexusd's own /metrics over the host network; Promtail
# tails .dev/*.log (`make run`/`make signerd` already write there via
# NEXUS_LOG_FILE) — neither needs nexusd running inside Docker.

observability-up: ## start prometheus + alertmanager + cadvisor + loki + promtail + grafana, pre-wired with the Nexus dashboards + alert rules -- run `make docker-up` first (not `make run`) if you want nexusd/signerd's own CPU/memory/network/disk IO to show up in cAdvisor
	docker compose -f deploy/docker-compose.observability.yml --profile observability up -d
	@echo "prometheus:   http://localhost:9091"
	@echo "alertmanager: http://localhost:9094"
	@echo "cadvisor:     http://localhost:8095"
	@echo "loki:         http://localhost:3101"
	@echo "grafana:      http://localhost:3310  (admin / nexus-dev-password) -- Nexus folder has Golden Signals + Cost & Tools + Infrastructure + Logs dashboards"

tempo-up: ## start Grafana Tempo (generic OTLP trace storage, separate opt-in profile -- pairs with the SAME Grafana `make observability-up` runs, but neither command requires the other) -- pair with NEXUS_OTLP_ENDPOINT=localhost:4418 (docs/observability.md), no headers/URL path needed unlike Langfuse's OTLP endpoint
	docker compose -f deploy/docker-compose.observability.yml --profile tracing up -d
	@echo "tempo: http://localhost:3200  (OTLP grpc:4417 http:4418) -- browse via Grafana's Explore, Tempo datasource"

profiling-up: ## start Grafana Pyroscope (continuous profiling storage, a THIRD separate opt-in profile -- pairs with the SAME Grafana `make observability-up` runs, but neither command requires the other) -- pair with NEXUS_PYROSCOPE_ADDR=http://localhost:4040 (docs/observability.md); nexusd/signerd PUSH profiles to it, so it works whether they run via `make run` or `make docker-up`
	docker compose -f deploy/docker-compose.observability.yml --profile profiling up -d
	@echo "pyroscope: http://localhost:4040  (own UI, or browse via Grafana's Explore/Profiles, Pyroscope datasource) -- allow ~60s after first start for /ready"

observability-down: ## stop the prometheus + alertmanager + cadvisor + loki + promtail + grafana (+ tempo/pyroscope, if up) containers
	docker compose -f deploy/docker-compose.observability.yml --profile observability --profile tracing --profile profiling down

# --- On-demand profiling: pprof (docs/observability.md) --
# `run`/`signerd` above already start this listener by default
# (NEXUS_PPROF_ADDR/NEXUS_SIGNERD_PPROF_ADDR), loopback-only -- these three
# just wrap `go tool pprof` against it so nobody has to remember the URL.
# PPROF_ADDR defaults to nexusd's port (127.0.0.1:6060); point at signerd's
# instead with `PPROF_ADDR=127.0.0.1:6061 make pprof-heap`. Each drops into
# pprof's own interactive shell (`top`, `web`, `list <func>`, ...).

pprof-cpu: ## capture a 30s CPU profile from nexusd's (or PPROF_ADDR=host:port's) pprof listener
	go tool pprof "http://$(PPROF_ADDR)/debug/pprof/profile?seconds=30"

pprof-heap: ## capture a heap (in-use memory) profile from nexusd's (or PPROF_ADDR=host:port's) pprof listener
	go tool pprof "http://$(PPROF_ADDR)/debug/pprof/heap"

pprof-goroutine: ## capture a goroutine dump from nexusd's (or PPROF_ADDR=host:port's) pprof listener -- the fastest way to spot a leak or a stuck call
	go tool pprof "http://$(PPROF_ADDR)/debug/pprof/goroutine"
