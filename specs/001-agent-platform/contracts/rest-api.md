# Contract: REST, SSE, webhook and CLI surfaces

**Plan**: [../plan.md](../plan.md) | **Spec**: FR-026, FR-027, FR-071, FR-079, FR-102, FR-117–FR-124 | **Source**: `internal/surfaces/{rest,cli,telegram,zalo,email}`, `cmd/nexusd/serve.go`

Surfaces are **thin translators**: they authenticate, create a session, hand it to `rest.RunStarter` (implemented only by
`kernelRunStarter`), and forward what comes back. They hold no control flow and never import `kernel/`.

Default listen address **`:8085`** (`NEXUS_HTTP_ADDR`; `cmd/nexusd/config.go`, `deploy/docker-compose.yml`, the CLI's and the web app's
default base URL — all agree). It was `:8080` until the chat-UI commit (`227b8f4`). **Docs drift**: `CLAUDE.md` and `README.md` still say
`:8080` and `.env.example` says `:8055`; the code is the authority. (The `CLAUDE.md` trap about a stray container publishing the port
still applies — check `docker ps`.) All bodies are JSON; **errors are plain-text `http.Error` bodies**, not a JSON envelope.

## 1. Authentication (FR-027)

Every `/v1/*` REST route is wrapped **individually** in `authMiddleware` — an *unregistered* path still gets the mux's native `404`
without needing a token (so "404 means unmounted" stays a usable conformance technique).

- Header: `Authorization: Bearer <token>`. Missing/malformed → `401`; a token the verifier rejects → `401`; a nil verifier →
  `500 server misconfigured` (**fails closed; never falls back to a client-supplied header**).
- The principal `(tenantID, userID)` comes **only** from verified claims (`PrincipalVerifier.Verify(token)`); the dev issuer is an
  Ed25519 JWT (`nexusd token`, `make token`); swapping in per-tenant OIDC is a wiring change in `cmd/nexusd`.
- **Audience gate**: a run's events, state and approvals are readable only by the run's submitting user (`403` otherwise).
- **CORS**: the request `Origin` is reflected; `OPTIONS` preflight → `204`, methods `GET, POST, OPTIONS`. Safe because auth is a bearer
  token attached deliberately, never a cookie (research R-23).

## 2. Run endpoints

| Method & path | Mounted | Purpose |
|---|---|---|
| `POST /v1/runs` | always | create + queue a run → **`202`** `{"run_id"}` |
| `GET /v1/runs/{id}` | always | `{run_id, status, terminal_reason?}`; `404` unknown, `403` not yours |
| `GET /v1/runs/{id}/events` | always | SSE stream (§3) |
| `GET /v1/sessions` | always | the caller's sessions: `[{run_id, status, terminal_reason?, created_at, autonomy_level}]` |
| `POST /v1/runs/{id}/cancel` | `RunCtl` wired | body `{reason}` — sole producer of `aborted` |
| `POST /v1/runs/{id}/steer` | `RunCtl` wired | body `{input}` — drained at a turn boundary; on an `awaiting_input` session it **transparently resumes** it |
| `POST /v1/runs/{id}/autonomy` | `RunCtl` wired | body `{target}` — tighten only |
| `POST /v1/runs/{id}/fork` | `RunCtl` wired | body `{at_seq, model?}` → `{session_id, digest_diverged, parent_digest_hex, child_digest_hex}` |
| `GET /v1/approvals`, `GET /v1/approvals/{id}` | `Oversight` wired | `ApprovalView` |
| `POST /v1/approvals/{id}/grant` | `Oversight` wired | body `{modified_input?}` — present ⇒ `granted_modified` |
| `POST /v1/approvals/{id}/deny` | `Oversight` wired | body `{reason}` |
| `POST /v1/sessions/{id}/content-access-grants` | `Grants` wired | body `{grantee_id, reason, ttl_seconds?}` → **`201`** `{grant_id, session_id, grantee_id, reason, expires_at}` |
| `GET /v1/sessions/{id}/content-access-grants/read` | `Grants` wired | plaintext under a live grant; `403` if none |

Optional-port routes are mounted only when their port is non-nil (`Oversight`, `Grants`, `RunCtl`) — the nil-valid convention.

### `POST /v1/runs` body

```json
{ "input": "required, non-empty",
  "data_label": "internal",          // default `internal`; feeds the router
  "difficulty": "simple",            // default `simple`; feeds the router
  "autonomy": "supervised",          // read_only | supervised | autonomous; default supervised; anything else → 400
  "budget_usd": "0.05",              // optional decimal string (cost.ParseDecimal, never a float) → session-scoped ceiling
  "conversational": false }          // true ⇒ a plain reply pauses to awaiting_input instead of terminating
```

`400` on invalid JSON, empty `input`, or an invalid `autonomy`. Admission goes through `controlplane.Port.AdmitRun`
(see [control-plane-v1.md](control-plane-v1.md)); the harness digest is computed and pinned **before** the session is created.

### `ApprovalView`

`{approval_id, session_id, tool_id, ask_kind, status, context, expires_at, created_at}` — `context` is the decision-ready package
(recipient/subject/attachment **digests**, effect class — never a bare identifier). Grant/deny/resume return
`{session_id, events_appended, error?}`.

## 3. SSE: `GET /v1/runs/{id}/events`

Headers: `content-type: text/event-stream`, `cache-control: no-cache`, `connection: keep-alive`.

**Frame**: `event: <name>\ndata: <json>\n\n`. Names:

| `event:` | `data` | Notes |
|---|---|---|
| *every durable event type* (`content`, `tool_use`, `tool_result`, `approval_requested`, `terminal`, …) | `eventDTO` | see below |
| `delta` | live-preview fragment | **best-effort**; no `seq`; never replayed; never durable (FR-028). Reasoning is signalled but its bytes are stripped |
| `error` | `{"error": "…"}` | stream ends after it |

```json
// eventDTO
{ "event_id": "…", "seq": 7, "type": "tool_result", "actor": "tool",
  "tool_id": "platform/file_read@v1", "pair_ref": "<tool_use event_id>", "model_id": "…",
  "created_at": "RFC3339Nano", "body": { /* decrypted payload; omitted for `thought` */ } }
```

**Ordering guarantees**: the handler **subscribes before replaying history**, then replays everything stored, then relays live events,
discarding any live event with `seq <= lastReplayedSeq` (seq is strictly sequential per session, so "already replayed" is exactly that
comparison — no gap or duplicate is possible). The stream **closes after the `terminal` event**. A *conversational* session emits
`awaiting_input` and **no** `terminal` until it is cancelled or times out. `thought` events never carry a body, even to the audience
("round-tripped, never shown"). Across processes, events and deltas fan out over the event bus (Redis Pub/Sub), so an SSE client may be
served by a different `nexusd` than the worker executing the run.

## 4. Inbound webhooks (FR-120, FR-121)

All three verify authenticity **before the body is read/parsed**, in constant time, then apply a limiter, then resolve a deterministic
per-peer session key (`telegram:{chat_id}`, `zalo:{sender_id}`, `email:{lower(from)}`): resume an `awaiting_input` session via
`Resumer`, else start a fresh **conversational** one reusing the key.

| Path | Authenticity | Body |
|---|---|---|
| `POST /v1/webhooks/telegram/{tenant_id}` | `X-Telegram-Bot-Api-Secret-Token` == tenant's sealed secret | Telegram update |
| `POST /v1/webhooks/zalo/{tenant_id}` | `X-ZEvent-Signature` = HMAC-SHA256 over the **raw body** with the tenant's app secret | Zalo event |
| `POST /v1/webhooks/email/{tenant_id}` | HTTP Basic Auth vs the tenant's webhook credential | `{from, subject?, text_body}` (≤ 1 MiB) |

Failures: `400` bad tenant id / body, `401` bad credential/signature, `404` no channel configured, `429` rate limited.

> **Not shipped (spec Known deviation 5):** no replay window (no timestamp/nonce check) on any webhook, and the limiter is keyed by
> `tenant_id`, not by external sender — so FR-120's "bounded replay window" and "per external identity" are **not met**.

Replies: the model's `content` and approval requests are delivered back through the **outbox** (event appended before the send;
idempotent on `(session, seq, surface, recipient)`); the payload each `Sender.Send` interprets carries an explicit `{kind, …}`
discriminator. **Cron** is outbound-only by design (no human peer to resume).

## 5. Operational endpoints (unauthenticated, on the same listener)

| Path | Purpose |
|---|---|
| `GET /healthz` | **liveness** only |
| `GET /readyz` | **readiness**: Postgres + Redis + the queue's consumer group + the `signerd` socket all answer |
| `GET /metrics` | Prometheus text: golden-signal gauges (`internal/obs.GoldenSignals`) plus `nexus_tool_call_count`, `nexus_model_input_tokens`, `nexus_model_output_tokens`, `nexus_model_cost`. The **only** place `cost.Money` micros become a float, at the exposition boundary |

`/metrics` is **unauthenticated** (docs/observability.md lists this as a caveat) — do not publish the port.

## 6. Command-line surfaces

**`nexusctl`** (`internal/surfaces/cli`; reads `NEXUS_HTTP_ADDR` — default `http://localhost:8085` — and `NEXUS_TOKEN`):
`run "<input>" [--autonomy=read_only|supervised|autonomous] [--budget=<usd>]`, `approvals {list|show|grant|deny}`, `cancel`, `steer`, `fork`. Same kernel, zero kernel changes (FR-082): the same task through REST and
the CLI must yield identical event sequences and terminal reasons (SC-018).

**`nexusd`** subcommands: `serve` (default; `--dev` enables the fake provider and auto-generated keys, otherwise it **fails closed**
listing every missing setting), `migrate` (direct to Postgres), `seed` (tenant + price book + skills), `token`, `verify-chain`,
`dashboard`, `go-live`, `erase` (crypto-shred), `ingest` (documents).

**`signerd`**: see [signer-protocol.md](signer-protocol.md).

## 7. Capability descriptors (the conformance seam)

Each surface registers a `capability.Descriptor`; `capability_test.go` runs the same conformance checks per surface (the "404 means
unmounted" technique for optional routes; the per-turn `Principal`). The web app, MCP, Telegram, Zalo, email, cron, REST and CLI are the
eight surfaces; a scheduler uses `PrincipalKind = scheduler`, everything else `user`.
