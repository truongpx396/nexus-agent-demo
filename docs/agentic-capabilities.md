# Agentic capabilities: Crawl4AI, OpenSandbox, and the skill/subagent bootstrap

`docs/build-phases.md`'s Phase 16 is the plan; this is the how-to. Two real, actively
maintained open-source projects fill two seams this codebase already left open on
purpose, plus the previously-empty skill bundle directory and a documented
`delegate` convention that makes both demonstrable end to end.

- **[Crawl4AI](https://github.com/unclecode/crawl4ai)** → `platform/web_crawl`
  (`internal/tools/builtin/web_crawl.go`), a sibling of the existing
  `platform/web_fetch` that returns rendered, boilerplate-stripped Markdown instead
  of raw HTML.
- **[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)** →
  `NEXUS_SANDBOX=opensandbox` (`internal/sandbox/opensandbox.go`), a second
  `sandbox.Isolation` backend for `platform/shell`, alongside the existing `docker`
  one.

Neither is a new architectural idea — see `docs/build-phases.md` Phase 16's own
intro for why.

## Architecture

```
nexusd --(HTTP, POST /md, Bearer token)--> crawl4ai (Docker, :11235)
nexusd --(OpenSandbox Go SDK, OPEN-SANDBOX-API-KEY)--> opensandbox-server (Docker, :8090)
                                                          |
                                                          `--(Docker socket)--> per-call sandbox containers
```

Both run as opt-in services in `deploy/docker-compose.agentic.yml` — a **third**
compose file, the same append-only pattern `deploy/docker-compose.local-llm.yml`
already established (`docs/local-llm.md`): nothing in it references
postgres/pgbouncer/redis/signerd/nexusd, so `make down` and `make llm-down` never
touch it, and a demo that never runs `make agentic-up` pays nothing.

`nexusd` itself keeps running as a plain host binary (`make run`) — neither service
needs `nexusd` to be containerized. OpenSandbox's server needs the **host Docker
socket** to create sandboxes as its own siblings; `internal/sandbox/opensandbox.go`
sets `ConnectionConfig.UseServerProxy: true`, so a host-run `nexusd` reaches every
sandbox's exec/egress API proxied back through the server's one published port
(`:8090`) instead of needing the server's internal dynamic sandbox-port range
published to the host too.

## Setup

```bash
# 1. Start both services
make agentic-up
# crawl4ai:    http://localhost:11235
# opensandbox: http://localhost:8090

# 2. platform/web_crawl — point nexusd at the crawler
export NEXUS_CRAWL4AI_URL=http://localhost:11235
export NEXUS_CRAWL4AI_API_TOKEN=nexus-dev-crawl4ai-token   # matches the compose file's dev secret

# 3. NEXUS_SANDBOX=opensandbox — an alternative to NEXUS_SANDBOX=docker for platform/shell
export NEXUS_SANDBOX=opensandbox
export NEXUS_OPENSANDBOX_URL=http://localhost:8090
export NEXUS_OPENSANDBOX_API_KEY=nexus-dev-opensandbox-key  # matches the compose file's dev secret

# 4. Seed a tenant — admits the two demo skills below into its config
make seed TENANT=acme

# 5. Run it
make run
```

The two demo skill bundles under `skills/` are trusted by default — `nexusd` ships
with a fixed dev signing public key as `NEXUS_SKILLS_SIGNING_PUBKEY`'s default (see
`cmd/nexusd/main.go`'s `devSkillsSigningPubkey`), the same "fixed, low-entropy
dev-only value" spirit as every other secret on this page. `NEXUS_SKILLS_ROOT`
defaults to the repo's own `skills/` directory (not `.dev/skills` — skill bundles
are durable, reviewed content like `evals/corpus/*.yaml`, not a machine-local
generated secret, so they belong in version control).

## Try it

```bash
TOKEN=$(make -s token TENANT=acme)
nexusctl run --autonomy supervised \
  "research the top headline on https://example.com and summarize it, citing the URL"
```

The transcript should show `activate_skill("web-research")`, then a
`platform/web_crawl` call, then a summary citing the crawled URL.

```bash
NEXUS_SANDBOX=opensandbox nexusctl run --autonomy autonomous \
  "delegate to a sandbox-runner sub-agent: run 'python3 --version' and report it"
```

The transcript should show a `platform/delegate` call with
`scope_grant: ["platform/shell@v1", ...]`, and the child session's `platform/shell`
call routed through `internal/sandbox/opensandbox.go` — check the child's own event
log for the `platform/shell` `tool_result`.

The web app's suggested-prompt chips (`web/src/lib/suggestedPrompts.ts`) reproduce
both without typing.

## What "subagent" means here

There is no per-agent system prompt or persona table in this codebase, and Phase 16
deliberately does not add one — `harness_digest` and prompt cache-stability
(`docs/build-phases.md` patterns #7/#28) make a per-agent system-prompt fork exactly
the kind of thing this project's own "seams decided early, never bolted on later"
rule warns against. A "subagent" here is just `platform/delegate` (an ordinary tool,
README task 8.9) called with a `scope_grant` — a provable subset of the caller's own
held tools, re-derived server-side, never trusted from the call — that names the
capability surface a particular kind of task needs. This page documents two such
conventions as `agent_id` labels for the audit trail:

| `agent_id` | `scope_grant` | For |
|---|---|---|
| `web-researcher` | `platform/web_crawl@v1`, `platform/web_fetch@v1`, `platform/activate_skill@v1` | Research questions |
| `sandbox-runner` | `platform/shell@v1`, `platform/activate_skill@v1` | Run-and-check tasks |

Both are also referenced from the matching skill's `trigger_hint`/`GUIDE.md`
(`skills/web-research/`, `skills/sandboxed-code/`) so the top-level agent reaches
for the right convention on its own.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `NEXUS_CRAWL4AI_URL` | unset (tool not registered) | Crawl4AI server base URL |
| `NEXUS_CRAWL4AI_API_TOKEN` | unset | Sent as `Authorization: Bearer` to Crawl4AI's `/md` endpoint |
| `NEXUS_SANDBOX=opensandbox` | unset (unsandboxed) | Routes `platform/shell` through OpenSandbox instead of Docker (`docker` is the other valid value) |
| `NEXUS_OPENSANDBOX_URL` | unset | OpenSandbox server base URL (`host:port`, or `http(s)://host:port`) |
| `NEXUS_OPENSANDBOX_API_KEY` | unset | Sent as `OPEN-SANDBOX-API-KEY` |
| `NEXUS_SKILLS_ROOT` | `skills` | Where `skills.LoadBundles` reads bundles from |
| `NEXUS_SKILLS_SIGNING_PUBKEY` | the fixed dev pubkey signing `skills/*` | base64 ed25519 public key; override to trust your own signed bundles instead |

## Regenerating skill signatures

Editing anything under `skills/*/` (including `GUIDE.md`) changes `BundleDigest`, so
the embedded `signature` in that skill's `skill.json` must be regenerated — otherwise
`nexusd` logs `skipped a skill bundle with a missing or invalid signature` and drops
it silently (skill bundles are less-trusted content than a first-party builtin, task
7.5's own posture). This demo's signing key is a fixed, deterministic dev value (not
a secret worth protecting — only the public half is ever checked at runtime); a real
deployment signs offline with a real, private key instead. To re-sign after an edit:

```go
// go run this against the repo root (module-local: it imports internal/skills)
seed := sha256.Sum256([]byte("nexus-agent-demo dev skills signing key v1"))
priv := ed25519.NewKeyFromSeed(seed[:])
bundles, _ := skills.LoadBundles("skills")
for _, b := range bundles {
    sig := ed25519.Sign(priv, skills.BundleDigest(b))
    fmt.Printf("%s: %s\n", b.SkillID, base64.StdEncoding.EncodeToString(sig))
}
```

Paste the printed base64 value into that skill's `signature` field.

## Caveats

- **Crawl4AI renders pages with a real headless Chromium.** The first crawl after
  `make agentic-up` is slow (cold browser pool) and every crawl is meaningfully
  slower than `platform/web_fetch`'s plain GET — `WebCrawl`'s own HTTP client uses a
  45s timeout for this reason. Use `web_fetch` for anything that doesn't need
  JS rendering.
- **OpenSandbox's server needs the host Docker socket** to create sandboxes as its
  own siblings (`deploy/docker-compose.agentic.yml` mounts
  `/var/run/docker.sock`) — a privileged dependency, named here rather than hidden.
  Run it only on a host whose Docker daemon you're comfortable handing to this
  service, the same posture `docs/local-llm.md`'s own Caveats section already models
  for its own fixed dev secrets.
- **There is no PID-count limit on the OpenSandbox backend**, unlike
  `NEXUS_SANDBOX=docker`'s `PidsLimit` — see `internal/sandbox/opensandbox.go`'s
  `resourceLimits` doc comment. Its wall-clock timeout and cpu/memory ceilings are
  what it actually enforces.
- **Every secret on this page is a fixed, low-entropy dev-only value** (the
  `CRAWL4AI_API_TOKEN`/`api_key` baked into `deploy/docker-compose.agentic.yml`, and
  the skill-signing key above) — the same spirit as this repo's other dev
  compose files. CHANGEME outside a local demo.
