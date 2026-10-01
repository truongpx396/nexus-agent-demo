# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

A single-binary Go reimplementation of `truongpx396/nexus-agent` (a multi-tenant, production-grade AI agent platform) that keeps every architectural seam and control but collapses deployment topology. Go 1.25, Postgres + PgBouncer + Redis, React web app in `web/`.

## Commands

```sh
make build                      # bin/nexusd, bin/nexusctl, bin/signerd
go test ./...                   # = make test: unit + property tests, no external services
go test ./kernel -run TestName -v                                  # single test
go test -race -tags=integration ./...                              # integration (CI runs this)
go test -tags=integration ./tests/integration -run TestName -v     # single integration test
golangci-lint run --build-tags=integration ./...                   # what CI runs (v2.5.0)
make eval                       # eval release gate (also a CI job)
make pr-size                    # changed lines vs origin/main; CI's pr-size workflow fails above 1000 (see Conventions)
go build ./... && go vet ./... && golangci-lint run --build-tags=integration ./... && go test ./...   # pre-"done" check
```

- `make lint` omits `--build-tags=integration`, so it skips integration-tagged files that CI lints. Use the command above. CI pins golangci-lint v2.5.0; a newer local binary can report findings CI won't.
- Integration tests start their own disposable Postgres + PgBouncer (testcontainers): Docker must be running, `make up` is not needed. They are the fastest, cleanest signal when runtime behavior looks wrong — prefer them over debugging the long-lived local stack.
- Live-model evals are behind the `liveeval` build tag. Correctness tests never call a live model; they use `internal/provider/fake`.
- CI also runs `govulncheck`, and `npm run lint` + `npm run build` in `web/`.

Run locally: `make up && make migrate && make seed && make run`, then `NEXUS_TOKEN=$(make -s token) ./bin/nexusctl run "..."`. `make run` starts signerd + `nexusd --dev` (fake provider, auto-generated keys in `.dev/`); without `--dev`, `serve()` fails closed on any unset security-critical config. Config is `NEXUS_*` env vars; `.env` is auto-loaded by `internal/dotenv` (a real env var always wins) and `.env.example` documents every variable. Ports: Postgres 5433 (direct — admin/migrations only), PgBouncer 6432 (runtime), Redis 6380, nexusd 8080. Observability/LLM/pprof stacks have their own `make` targets (see Makefile `##` comments and `docs/`).

Web: `cd web && npm ci && npm run dev | lint | build` (oxlint, `tsc -b`).

**Trap:** a stray Docker container (`nexus-agent-demo-nexusd-1`) can publish :8080 and silently shadow the native `nexusd` — symptoms look like real regressions (404 on a new route, 401 on a fresh token). Check `docker ps` before assuming a code change broke something. A stack reset: `docker compose -f deploy/docker-compose.yml down -v`, regenerate `.dev/*.key`, `make up && make migrate && make seed`.

## Governing documents

`.specify/memory/constitution.md` holds the nine principles and is the code-review checklist; where a plan or design conflicts with it, the constitution wins. `docs/constitution.md` holds the same text (plus its amendment-history comment) and is cited by code comments — keep the two in sync. Read it before design or architecture changes. Spec Kit skills (`/speckit-specify`, `-plan`, `-tasks`, `-implement`, …) live in `.claude/skills/`.

`README.md` is the architecture narrative; `docs/build-phases.md` is the build ledger. Code comments and test names cite "Phase N" / "task N.M" from those two files.

## Architecture

`nexusd` is one process containing the control plane, the data plane, and the REST surface. The control-plane/data-plane split is a Go interface (`internal/controlplane.Port`), not a network boundary, so a physical split later is a `main.go` change.

**Run path.** A surface (`internal/surfaces/rest`, `nexusctl` via `internal/surfaces/cli`, plus Telegram/Zalo/email/cron/MCP) only translates I/O. `POST /v1/runs` goes to the `rest.RunStarter` interface; its only implementation is `kernelRunStarter` in `cmd/nexusd/runner.go`, which is the one place a `kernel.RunState`/`RunConfig` is built. `kernel.Kernel` (`kernel/loop.go`) is the only place agent control flow lives: an `iter.Seq2` generator over `provider.Provider` (one port, one normalized stream; `fake/`, `anthropic/`, router, failover) and `ToolExecutor` (`internal/tools` 16-step pipeline, `internal/permissions` 10-layer chain). Cancel/steer/resume/fork go through `internal/runctl`; `internal/queue` is the durable job queue with per-session-key serial locking, drained by workers started in `cmd/nexusd/serve.go`.

**State.** Conversation state is an append-only event log (`kernel/events.go`, `internal/store`): a trigger rejects UPDATE/DELETE for every role, payloads are envelope-encrypted per tenant (`internal/crypto`; erasure is crypto-shredding, never deletion), and derived tables are projections of the log. Every appended event also gets a hash-chained audit receipt signed by `signerd` over a unix socket — only `cmd/signerd` may hold the private key, and `nexusd` cannot append events unless signerd is reachable. Every `tool_use` must be paired with a `tool_result` before the next model call (property-tested in `tests/property/`).

**Tenant isolation.** Two DB roles: `nexus` (superuser, direct to Postgres, migrations only) and `nexus_app` (NOBYPASSRLS, through PgBouncer in transaction pooling, everything at runtime). Scoping is transaction-local — `set_config('app.tenant_id', $1, true)` in `internal/store/tenant.go` — never session-level, because the pooler reassigns connections between tenants. Every tenant table has RLS enabled **and forced**.

### Rules that are enforced mechanically (so follow them up front)

- **Import boundaries** (`tests/contract/boundaries_test.go`; the check is transitive, so an indirect import also fails): `kernel/` must not import `internal/surfaces` or `internal/controlplane`; surfaces must not import `kernel/`; `controlplane` must not import `sandbox`/`memory`/`provider`; the three `cmd/*` binaries must not import each other; `cmd/nexusd` must not import `internal/audit/signerkey` (the private-key package — nexusd signs only through `audit.SignerClient`). `cmd/nexusd` is the composition root and the one package allowed to import both `kernel` and the surfaces.
- **Exhaustive switches**: switches over `kernel.TerminalReason` and response classification must list every case — `exhaustive` runs with `default-signifies-exhaustive: false`, so a `default:` does not satisfy it. Add the case.
- **No float in money**: `internal/cost` uses integer money; `money_notfloat_test.go` fails on any `float32`/`float64` token in the package.
- **File size**: a non-test Go file may not exceed 500 lines (`tests/contract/filesize_test.go`; `_test.go` and generated files are exempt, and `lineCapExempt` is empty on purpose). When you cross it, split by responsibility — see Conventions.
- **Lint bans**: `fmt.Print*` outside `cmd/` (log with zerolog via `internal/obs`; the lint message says slog, but the codebase uses zerolog) and `math/rand` anywhere (use `crypto/rand`).
- **Telemetry is content-free**: span attributes pass a deny-by-default allowlist (`internal/obs/allowlist.go`). Never put prompt or tool content into spans or logs.
- **Optional `Kernel` hooks are nil-valid** (`Receipts`, `OnSuspend`, `OnDelegate`, `Stuck`, …): nil means "that control isn't wired", which is how minimal kernels are built in tests. New hooks must follow that convention.

## Conventions

- Split large Go files by responsibility **within the same package** (e.g. `cmd/nexusd`: `serve.go`, `ports.go`, `cli_*.go`; `kernel`: `turns.go`, `events.go`, `terminal.go`) — never into a new package or sub-package; the boundary test is package-granular and the repo already follows this. Aim for ~150–400 lines per file (500 is the enforced cap, above), copy the full import block and run `goimports -w` to prune. `kernel/turns.go` → `kernel/metering.go` is the worked example: a pure move, verified with `go build` and the package's tests.
- Keep PRs under **1,000 changed lines** (insertions + deletions; lockfiles, `web/dist`, `evals/testdata/baseline.json` and the vendored Spec Kit scaffolding don't count). `make pr-size` measures your branch; `.github/workflows/pr-size.yml` fails the PR above the limit. Split a bigger feature into stacked PRs. A change that is big but mechanical (a rename, a file split) gets the `large-pr` label and a sentence in the description saying why. The limit and exclusions live in `scripts/pr-size.sh`.
- **Opening a PR is not done until CI is checked.** After `gh pr create`, and after every later push to the PR, run `gh pr checks <n> --watch` (in the background if you have other work) and report each check's outcome. `skipped` is not `passed`: `unit`, `integration` and `eval-gate` are skipped whenever `lint` fails, because they `need` it. On a failure, read `gh run view <run-id> --log-failed` and say whether this PR caused it or `main` was already red (`gh run list --branch main --workflow ci --limit 1`). Never write "CI is green" without having seen it. A `PostToolUse` hook on `gh pr create` injects this reminder with the PR number; pushes to an existing PR rely on this rule alone.
- Prompts, tools, skills and models are production config: changing one is a deploy and must clear the eval gate.

Path-specific rules for `migrations/` and `evals/` are in `.claude/rules/`.

## Claude Code setup

`.claude/settings.json` is shared: a permission allowlist for the verification commands above, deny rules for secrets (`.env`, `.dev/*.key`, `.dev/signer/`, `*.pem`), destructive commands (`docker compose … down -v`, force-push, `git reset --hard`) and hand-editing `evals/testdata/baseline.json`, plus three hooks in `.claude/hooks/`: after a Go edit, `goimports -w` and a nudge if the file passed the line cap; before `gh pr create`, an advisory PR-size check; after it, a reminder to verify CI. The hooks need `jq`, and `goimports` (`go install golang.org/x/tools/cmd/goimports@latest`) for formatting. Personal overrides go in `.claude/settings.local.json` (gitignored). Read denies stop the Read tool only, not `cat` through Bash.
