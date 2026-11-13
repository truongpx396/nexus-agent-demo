# Review findings and implementation guide (2026-11-13)

**Reviewed at**: `7b94aaa` (main). **Method**: the code was read; `go build ./...`, `go vet ./...` and `go test ./...` pass (51 packages). Integration tests, lint, the eval gate and live model calls were **not** run, so no finding below was reproduced. R1 was checked against the provider's published API documentation; everything else is marked *read*.

This document is written for whoever implements the fixes, including a model with no memory of the review. Each work item says what is wrong, where, what to change, which tests to write and when it is done. The open tasks are registered in `specs/001-agent-platform/tasks.md` (T266 to T283); this file holds the detail.

## How to use this document

1. Pick **one** work item. One item is one pull request. Do them in the order of the table below unless an item says it depends on another.
2. Read the files named under **Where** before changing anything. Line numbers drift, so items name functions and types. If a named function no longer exists or no longer does what the item says, stop and re-read the code: the item may already be fixed or the design may have moved. Do not force the change.
3. Write the failing test named under **Tests** first and watch it fail. That proves the finding still holds. If it passes before your change, say so in the pull request and close the task as not reproducible.
4. Make the change, then run the checks under **Ground rules**.
5. In the same pull request, tick the item's task in `specs/001-agent-platform/tasks.md` (`- [ ]` to `- [X]`) and make its line name every file you touched. A ticked task that names a missing file fails `tests/contract/specs_test.go`.

## Ground rules (they are enforced by tests and CI)

- Before saying a change is done, run: `go build ./... && go vet ./... && golangci-lint run --build-tags=integration ./... && go test ./...`. For anything that touches Postgres, Redis or the queue, also run the named integration test: `go test -tags=integration ./tests/integration -run TestName -v` (Docker must be running).
- A pull request stays under 1,000 changed lines (`make pr-size`).
- A change to non-test Go under `internal/`, `cmd/`, `kernel/` or `evals/`, to a migration, or to `web/src/` must also change something under `specs/` (`make spec-gate`). Ticking the task satisfies it.
- A non-test Go file may not exceed 500 lines. Split by responsibility **inside the same package**; never create a new package to make a file shorter.
- `kernel/` and the core packages listed in `tests/contract/boundaries_test.go` (they include `internal/provider`, `internal/queue`, `internal/cost`, `internal/reliability` and `internal/promptctx`) must not import `net/http`, a database or cache driver, or `os/exec`, even indirectly. I/O lives in adapter subpackages (`internal/provider/anthropic`, `internal/queue/redis`, ...). `tests/contract/boundaries_test.go` fails otherwise.
- No `fmt.Print*` outside `cmd/`; log with zerolog. No `math/rand` (use `math/rand/v2` for jitter as `internal/queue/worker.go` does, or `crypto/rand`). No package `sort`. No `float32`/`float64` anywhere under `internal/cost`. No identifier, file name or string literal containing "phase".
- Match errors with `errors.Is` / `errors.AsType`, wrap with `%w`, name sentinels `ErrX` and error types `XError`.
- A package that starts a goroutine and has tests needs `goleak.VerifyTestMain` (most already have a `leak_test.go`). A test that leaks must cancel its context in `t.Cleanup`.
- A new hook or optional dependency must be nil-valid: nil means "not wired", and every existing test that builds a bare struct keeps working.
- Every new `NEXUS_*` environment variable is documented in `.env.example`.
- A change to what the model is shown (prompt text, tool names or descriptions, context management) is a deploy: run `make eval` and say so in the pull request. Never hand-edit `evals/testdata/baseline.json`. Read `.claude/rules/evals.md` before touching `evals/`.
- A `*.gen.go` file is never edited by hand. A REST route change starts in `api/openapi.yaml`, then `make generate`, and the route's `bruno/*.bru` file is updated in the same change.
- Correctness tests never call a live model. Use `internal/provider/fake`, or an `httptest.Server` for adapter tests as `internal/provider/anthropic/anthropic_test.go` does.

## Work items

| Id | Task | Title | Severity | Depends on |
|---|---|---|---|---|
| R1 | T266 | Send tool names the provider API accepts | blocker: every hosted call with tools is rejected | none |
| R2 | T267 | Pin tenant memory and per-run tools in the log and apply them on every resume | blocker: memory and MCP tools never reach the model | none |
| R3 | T268 | Make `max_tokens` a setting with a workable default | blocker: long outputs end the run | none |
| R4 | T269 | Retry a throttled or overloaded provider call, classified and backed off | blocker: one transient error fails the run | none |
| R5 | T270 | Serialise each content block with exactly its own fields | likely blocker; verify first | none |
| R6 | T271 | Pruning must keep `tool_result` structure | high (only when pruning is on) | none |
| R7 | T272 | Condensation: pair-safe cut, real summariser input, durable across resume | high (only when condensation is on) | R6 |
| R8 | T273 | Cache the conversation history, not only the system prompt | high (cost) | R5 |
| R9 | T274 | Sandbox fails closed outside `--dev`; the unsandboxed shell gets a clean environment | blocker (security) | none |
| R10 | T275 | Egress client: check redirects and the address actually dialled | high (security) | none |
| R11 | T276 | `/metrics` behind a token and cached; admin DSN required outside `--dev` | high (security) | none |
| R12 | T277 | Verify bearer tokens with a public key file | medium (security) | none |
| R13 | T278 | Redis connection with authentication and TLS | medium (security) | none |
| R14 | T279 | Queue: extend the lease while a job runs; a stale job is a no-op; the breaker recovers | blocker (reliability): any run over two minutes | none |
| R15 | T280 | Periodic orphan recovery | high (reliability) | R14 |
| R16 | T281 | Drain in-flight runs on shutdown; recover a panic in a job | high (reliability) | none |
| R17 | T282 | Size a cost reservation from the prompt | medium (cost) | R3 |
| R18 | T283 | Test the real kernel and catalog against the provider's wire rules | high (it is why R1 to R5 went unseen) | R1, R5 |

---

### R1 — Send tool names the provider API accepts (T266)

**Problem.** The catalog shown to the model uses the internal reference as the tool name, for example `platform/shell@v1` or `mcp/github/list_prs@v1`. The Anthropic API requires a tool name to match `^[a-zA-Z0-9_-]{1,128}$`, and OpenAI-compatible APIs require `^[a-zA-Z0-9_-]{1,64}$`. `/`, `@` and `.` are not allowed, so a request with any tool is rejected with HTTP 400 before the model runs. The live evals never see this because they use names such as `get_weather` (`evals/live_corpus.go`).

**Where.**
- `internal/tools/identity.go`: `ToolRef.String()` builds `namespace/name@version`.
- `cmd/nexusd/pipeline.go`, `newToolPipeline`: `provider.ToolSchema{Name: d.ID.String(), ...}`.
- `internal/provider/anthropic/request.go`: `toAnthTools` (sends `s.Name`) and `toAnthMessages` (sends `b.ToolName` for a replayed `tool_use`).
- `internal/provider/anthropic/stream.go`: the `content_block_start` case stores `evt.ContentBlock.Name` and later emits it as `ChunkToolUse.ToolName`.
- `internal/provider/litellm/litellm.go`: the same three places (`Function.Name` when replaying, `Name: s.Name` in the tool list, `tb.name = tc.Function.Name` when streaming).

**Change.** Translate at the adapter boundary only. The kernel, the event log and the tool pipeline keep using the internal reference.

1. Add `internal/provider/wirename.go`:
   - `type WireNames struct { toWire, toRef map[string]string }`.
   - `func NewWireNames(tools []ToolSchema) (WireNames, error)`: for each tool compute `wire := sanitizeWireName(t.Name)`; if two different refs produce the same wire name return an error naming both; fill both maps.
   - `func (w WireNames) Wire(ref string) string`: the mapped name, or `sanitizeWireName(ref)` when the ref is not in the catalog (a replayed call to a tool that is no longer loaded).
   - `func (w WireNames) Ref(wire string) string`: the mapped ref, or `wire` unchanged when unknown.
   - `sanitizeWireName(ref string) string`: replace every byte outside `[a-zA-Z0-9_-]` with `_`. If the result is longer than 64 bytes, keep the first 55 bytes, then `_`, then the first 8 hex characters of the SHA-256 of the full ref. `platform/shell@v1` becomes `platform_shell_v1`.
2. In `anthropic.Provider.Stream`: build `names, err := provider.NewWireNames(tools)` (return the error), pass `names` to `toAnthTools` and `toAnthMessages`, store it on the `stream` struct, and in `content_block_start` set `name: names.Ref(evt.ContentBlock.Name)`.
3. Do the same in `internal/provider/litellm/litellm.go`.
4. Do not change `internal/provider/fake`: it works on internal references.

**Tests.**
- `internal/provider/wirename_test.go`: table test of `sanitizeWireName` (`platform/shell@v1`, a multi-segment `mcp/...` ref, a ref over 64 bytes, a version with a dot); round trip `Ref(Wire(x)) == x` for every catalog entry; a collision returns an error; every wire name matches `^[a-zA-Z0-9_-]{1,64}$`.
- `internal/provider/anthropic/anthropic_test.go`: with a tool named `platform/shell@v1`, the request body captured by the `httptest.Server` carries `platform_shell_v1` in `tools[0].name` and in a replayed `tool_use` block, and an SSE `tool_use` block named `platform_shell_v1` comes back as `ChunkToolUse.ToolName == "platform/shell@v1"`.
- The equivalent test in `internal/provider/litellm/litellm_test.go`.

**Done when.** The tests pass; `grep -rn "Name: s.Name" internal/provider` finds no adapter sending a raw name; `make eval` passes (the names the model sees changed).

---

### R2 — Pin tenant memory and per-run tools in the log and apply them on every resume (T267)

**Problem.** `kernelRunStarter.StartRun` (`cmd/nexusd/runner.go`) builds a `kernel.RunConfig` whose `System` includes the tenant's memory text and whose `Catalog` includes the request's MCP tools. That config is used only by `Kernel.Seed`, which writes the opening events and never calls the model. The turn loop runs later on a queue worker through `runctl.Control.Resume`, which builds its config from the process-wide values: `runstate.ConfigFor(sess, kernel.RunConfig{System: c.System, Catalog: c.Catalog, ...})`. So the model never sees the tenant's memory or the run's MCP tools, although the log records `memory_loaded` and `tool_loaded` for them. The same holds for the other resuming services.

**Where.**
- `cmd/nexusd/runner.go`: `StartRun` (memory folded into `system`, `ExtraCatalog` appended to `catalog`).
- `kernel/loop.go`: `seed` writes `toolLoadedPayload{ToolID}` and `memoryLoadedPayload{Sources}`; `kernel/events.go` defines both payloads.
- Call sites that build a resumed run's config: `internal/runctl/resume.go` (`Resume`), `internal/oversight/resume.go`, `internal/plan/steps.go`, `internal/delegate/resolve.go`. Each already has the session's history and a `kernel.DecryptFunc` in hand because it calls `kernel.Rehydrate`.
- `internal/runstate/config.go` and `config_test.go` (`runConfigFields` classifies every `RunConfig` field; a new field fails the test until it is listed).

**Change.** Make the log the source of truth, so a resume reads what the session was started with and the system prompt never changes mid-session (constitution Principle III).

1. `kernel/events.go`: add a `Text string` field with JSON tag `text,omitempty` to `memoryLoadedPayload`, and to `toolLoadedPayload` add `Description string` (tag `description,omitempty`) and `InputSchema json.RawMessage` (tag `input_schema,omitempty`). Payloads are sealed, so the text is encrypted at rest like any message.
2. `kernel/types.go`: add two `RunConfig` fields, `MemoryText string` and `ExtraCatalog []provider.ToolSchema`. Classify both as `freshOnly` in `runConfigFields` (`internal/runstate/config_test.go`); that test fails until they are listed. Its `populated` helper already handles a `string` and a `[]provider.ToolSchema`, so it needs no change.
3. `kernel/loop.go`, `seed`: put `cfg.MemoryText` into the `memory_loaded` payload. For each `toolID` in `cfg.LoadedTools`, if a schema in `cfg.ExtraCatalog` has `Name == toolID`, copy its `Description` and `InputSchema` into that `tool_loaded` payload. Resident tools keep an id-only payload.
4. Add `kernel/pinned.go`:
   - `type Pinned struct { MemoryText string; ExtraCatalog []provider.ToolSchema }`.
   - `func RehydratePinned(ctx context.Context, history []eventlog.Event, decrypt DecryptFunc) (Pinned, error)`: scan `history` for `eventlog.EventMemoryLoaded` (take `Text`) and `eventlog.EventToolLoaded` whose payload has a non-empty `InputSchema` (append a `provider.ToolSchema`). Use the existing `decodeEvent` helper. Decrypt only events of those two types; skip every other event without decrypting it.
   - `func (p Pinned) Apply(base RunConfig) RunConfig`: if `MemoryText != ""` set `base.System = base.System + "\n\n" + p.MemoryText`; append `p.ExtraCatalog` to a **copy** of `base.Catalog` (never append to the shared slice).
5. In each of the four resuming call sites, after `runstate.ConfigFor(...)`, call `kernel.RehydratePinned` with the history and decrypt function already loaded there, and use `pinned.Apply(cfg)`. An old session has no text or schema in its payloads, so `Apply` is a no-op and behaviour is unchanged.
6. `cmd/nexusd/runner.go`, `StartRun`: stop concatenating memory into `system`; set `MemoryText: snap.Text` and `ExtraCatalog: req.ExtraCatalog` on the config instead. Keep `MemorySources` and `LoadedTools` as they are.

**Tests.**
- `kernel/pinned_test.go`: seed a run on `internal/eventlog/inmem` with `MemoryText` and one `ExtraCatalog` entry, then `RehydratePinned` over the log's history returns both; a history without them returns the zero value; `Apply` does not modify the base catalog's backing array.
- `tests/integration/resume_config_test.go` (it already covers resumed configs): start a run through the launcher with a tenant memory file and an MCP catalog entry, let a worker drive it with a provider that records the `provider.Prompt` and tool list it was called with, and assert the recorded system prompt contains the memory text and the tool list contains the MCP schema. If `internal/provider/fake` cannot record its inputs, add an optional `OnStream func(Prompt, []ToolSchema)` field to it (nil-valid).

**Done when.** Both tests pass; `go test ./internal/runstate ./tests/contract` passes; `make eval` passes.

---

### R3 — Make `max_tokens` a setting with a workable default (T268)

**Problem.** The Anthropic adapter always sends `max_tokens: 4096` (`defaultMaxTokens` in `internal/provider/anthropic/anthropic.go`); nothing in `cmd/nexusd` sets `Provider.MaxTokens`. With adaptive thinking on, thinking tokens count against that limit. When the limit is hit, `runTurns` (`kernel/turns.go`, the `done == provider.DoneMaxOutput` branch) ends the run with `TerminalError`. A request to write one long file therefore fails the run.

**Change.**
1. `cmd/nexusd/provider.go`: add `func anthropicMaxTokens() int` reading `NEXUS_ANTHROPIC_MAX_TOKENS` with `envIntOr`, default `16000`, clamped to 1024..128000 (out of range logs a warning and uses the default, as `queueWorkers` does in `cmd/nexusd/workers.go`). In `newProvider`'s `anthropic` case set `p.MaxTokens = anthropicMaxTokens()`.
2. `cmd/nexusd/cost.go`, `newCostGate`: pass `cost.GateConfig{MaxOutputTokenEstimate: int64(anthropicMaxTokens())}` when `NEXUS_PROVIDER=anthropic`, so the worst-case reservation covers what the call may now produce. Leave the default for other providers.
3. Check `internal/provider/litellm/litellm.go` for a fixed output limit and apply the same pattern with `NEXUS_LITELLM_MAX_TOKENS` if one exists.
4. `kernel/turns.go`: keep the terminal reason, but make the detail say what to do: `"provider stopped at max_tokens before finishing its reply; raise NEXUS_ANTHROPIC_MAX_TOKENS or ask for a shorter output"`.
5. Document the variable in `.env.example`.

**Tests.** `cmd/nexusd`: a table test of `anthropicMaxTokens` (unset, valid, too small, too large). `internal/provider/anthropic/anthropic_test.go`: a provider with `MaxTokens: 16000` sends `"max_tokens":16000`.

**Owner decision.** The default (16000 here) and whether a `max_tokens` stop should continue the turn instead of ending the run. Do not implement continuation in this item.

---

### R4 — Retry a throttled or overloaded provider call, classified and backed off (T269)

**Problem.** `cmd/nexusd/compose.go` builds `provider.Wrap([]provider.Provider{prov})`: a failover chain of one. `Wrap` only moves to the *next* provider, so a 429, a 529 or a dropped connection on the only provider goes straight to `terminateFromStreamError` and the run ends with `error`. Constitution Principle VIII requires a failure to be classified, then retried with a logged reason and jittered backoff, and circuit-broken after three identical failures.

**Where.** `internal/provider/failover.go` (`ClassifyTrigger`, `Wrap`, `peekedStream`), `internal/provider/provider.go` (`ThrottleError`), `internal/provider/anthropic/stream.go` (`Stream`: the `client().Do` error path and the non-200 path), `internal/provider/anthropic/response.go` (`classifyAPIError`).

**Change.**
1. `internal/provider/provider.go`: add `RetryAfter time.Duration` to `ThrottleError` (zero means the provider gave none).
2. `internal/provider/anthropic/stream.go`: in the non-200 branch parse the `retry-after` response header (whole seconds) and set it on the returned `*provider.ThrottleError` (use `errors.AsType` on the result of `classifyAPIError`). In the `client().Do` error branch return `ctx.Err()` unwrapped when `ctx.Err() != nil`, so a cancelled run is not classified retryable.
3. Add `internal/provider/retry.go`:
   - `type RetryConfig struct { Attempts int; Base, Max time.Duration; Sleep func(ctx context.Context, d time.Duration) error; OnRetry func(attempt int, err error, delay time.Duration) }`. Defaults: `Attempts` 3 (total tries), `Base` 1s, `Max` 20s, `Sleep` a `time.NewTimer` select on `ctx.Done()`. `OnRetry` nil is valid.
   - `func WithRetry(p Provider, cfg RetryConfig) Provider`. Its `Stream` does what `failoverProvider.Stream` does for one provider: call `p.Stream`, then pull the first chunk with `Next`. On an error from either, retry only if `ClassifyTrigger(err) == TriggerRetryable`, attempts remain and `ctx.Err() == nil`. Delay is full jitter: a random duration in `[0, min(Max, Base<<attempt))` from `math/rand/v2`, raised to `RetryAfter` when the error carries one (capped at `Max`). Call `OnRetry` before sleeping. Return a `peekedStream` on success. Never retry after the first chunk has been returned and never retry `TriggerContextOverflow` or `TriggerPermanent`.
   - Implement `BoundModel() string` on the wrapper by delegating to `p` when it is a `ModelBound`, as `failoverProvider` does. Without it a run is priced under the routed model instead of the served one.
4. `cmd/nexusd/compose.go`: `Provider: provider.Wrap([]provider.Provider{provider.WithRetry(prov, provider.RetryConfig{OnRetry: logProviderRetry})})`, where `logProviderRetry` logs attempt, error and delay with zerolog at warn level.

**Tests.** `internal/provider/retry_test.go` with a scripted provider and a `Sleep` that records delays instead of sleeping: a throttle then success returns the stream after one retry; three throttles return the last error; a permanent error and a context-overflow error are not retried; an error after the first chunk is not retried; a `RetryAfter` of 5s yields a delay of at least 5s; a cancelled context stops the loop; `BoundModel` passes through. `tests/contract` must still pass: its metering check exempts `Stream` calls made inside `internal/provider`.

---

### R5 — Serialise each content block with exactly its own fields (T270)

**Problem (read, not reproduced; verify first).** `anthContentBlock` in `internal/provider/anthropic/request.go` is one struct for every block type with `omitempty` on every field. Two consequences:
- A `thinking` block whose text is empty is sent without its `thinking` field. On the models this adapter enables thinking for, thinking text is omitted by default, so the text is empty and only the signature is present. A `thinking` block without a `thinking` field is expected to be rejected on the next request of any tool-using run.
- A `tool_use` block whose `Input` is nil or empty is sent without `input`, which the API requires.

**Verify.** With an API key, run one tool-using task (`NEXUS_PROVIDER=anthropic`, after R1) and watch the second model call. Without a key, proceed: the change below is correct either way.

**Change.** Give `anthContentBlock` a `MarshalJSON` method that switches on `Type` and emits exactly:
- `text`: `type`, `text`.
- `thinking`: `type`, `thinking` (always, even when empty), `signature` (always).
- `tool_use`: `type`, `id`, `name`, `input` (the raw JSON, or `{}` when empty).
- `tool_result`: `type`, `tool_use_id`, `content` (omit when empty), `is_error` (omit when false).
- any optional `cache_control` field the struct carries (R8 adds one).
An unknown `Type` returns an error. Remove the now-unused `omitempty` tags or keep the struct tags only for decoding.

**Tests.** `internal/provider/anthropic/anthropic_test.go`: marshal one message per block kind and compare with exact JSON strings, including a thinking block with empty text and a `tool_use` with nil input.

---

### R6 — Pruning must keep `tool_result` structure (T271)

**Problem.** `promptctx.Prune` (`internal/promptctx/prune.go`) replaces an old, large `"tool"` message with `provider.TextMessage(m.Role, ...)`. That turns the message's `tool_result` blocks into one plain text block and drops every `ToolUseID`. The assistant message before it still holds the `tool_use` blocks, so the request now has calls with no results and the API rejects it. Pruning is off by default (`NEXUS_CONTEXT_PRUNING`), which is why this has not been seen. It also measures the whole message's joined text, not each result.

**Change.** In `Prune`, for a candidate `"tool"` message:
1. Copy its `Blocks` slice (`slices.Clone`); `out[i]` shares the backing array with the caller's transcript and must not be written through.
2. For each block with `Kind == provider.BlockToolResult`, apply the two thresholds to `len(b.Text)`: over `HardClearAt` set `b.Text = pruneMarker(original)`; over `SoftTrimAt` set `b.Text = original[:previewBytes] + "\n" + pruneMarker(original)`. Leave `ToolUseID`, `IsError` and `Kind` untouched.
3. Count pruned **blocks**, and only replace `out[i]` when at least one block changed.

**Tests.** `internal/promptctx/prune_test.go`: a transcript with an assistant message holding two `tool_use` blocks and a tool message holding two large results; after `Prune` the view passes `provider.ValidateTranscript`, both `ToolUseID`s are intact, the texts are shortened, and the input transcript is deeply equal to a copy taken before the call.

---

### R7 — Condensation: pair-safe cut, real summariser input, durable across resume (T272)

**Problem.** Four defects in `internal/promptctx/condense.go` and `kernel/turns.go`; condensation is off by default (`NEXUS_CONDENSE_THRESHOLD_BYTES`).
1. `ShouldCondense` cuts at `len(transcript) - 4` whatever is there. If the first retained message is a `"tool"` message, its `tool_use` calls were summarised away and the request is rejected.
2. The summary is inserted as an `"assistant"` message at the start of the transcript. A conversation must start with a user turn.
3. `CondensePrompt` builds its input with `line()`, which truncates every message to 256 bytes (`previewBytes`) and, through `PlainText`, shows nothing for a `tool_use` block. The summariser sees fragments.
4. The condensed transcript lives only in memory. `kernel.Rehydrate` ignores `EventCondensation`, and every queued start, approval and follow-up message rehydrates, so the full history returns and is condensed again with another model call.

**Change.**
1. `ShouldCondense`: after computing `covered`, step back while the first retained message is a tool message: `for covered > 0 && transcript[covered].Role == "tool" { covered-- }`. Return `false, 0` if `covered <= 0`.
2. Add one function in `kernel` (for example in `kernel/transcript.go`) used by both the live loop and replay: `func condensedTranscript(transcript []provider.Message, covered int, summary string) []provider.Message`, returning a new slice whose first message is `provider.TextMessage("user", "[summary of the earlier conversation]\n"+summary)` followed by `transcript[covered:]`. Use it in `runTurns` in place of the inline construction.
3. `CondensePrompt`: render each message with a new helper instead of `line()`: text and tool results up to 4096 bytes each, and a `tool_use` block as `[called <ToolName> with <input, up to 512 bytes>]`. Leave `ExtractivePass` and `line()` as they are.
4. `kernel/events.go`: add a `CoveredMessages int` field with JSON tag `covered_messages,omitempty` to `condensationPayload`; `runTurns` sets it to `covered`.
5. `kernel/rehydrate.go`, `Rehydrate`: add a case for `eventlog.EventCondensation`: call `flushAssistant()` and `flushResults()`, decode the payload, and if `0 < CoveredMessages <= len(transcript)` set `transcript = condensedTranscript(transcript, p.CoveredMessages, p.Summary)`. A payload with `CoveredMessages == 0` (written before this change) is skipped, which is today's behaviour.

**Tests.**
- `internal/promptctx/condense_test.go`: a transcript whose cut would land on a tool message is cut one earlier; the retained tail never starts with a tool message.
- `kernel/rehydrate_test.go`: drive a kernel on `internal/eventlog/inmem` with a scripted provider and a small `CondenseThresholdBytes` until one condensation happens, then `Rehydrate` the log's history and require the result to be deeply equal to the live `st.Transcript`, and to pass `provider.ValidateTranscript`.
- A second resume of the same log does not append another `EventCondensation`.

**Note.** Enabling either control by default is a separate task (spec 004 T035) and needs a live eval.

---

### R8 — Cache the conversation history, not only the system prompt (T273)

**Problem.** The only cache breakpoint is on the system block (`toAnthSystem` in `internal/provider/anthropic/request.go`). In an agent loop the growing message history is most of the input, and none of it is cached. A short system prompt plus catalog may also be below the model's minimum cacheable length, in which case nothing is cached at all. Principle III's target of more than 90% cache-read cannot be met this way.

**Change.**
1. Add a `CacheControl *anthCacheControl` field with JSON tag `cache_control,omitempty` to `messagesRequest` and set it to `{type: "ephemeral"}` in `Stream`. The API then places a breakpoint on the last cacheable block of each request, so each turn reads the prefix the previous turn wrote.
2. Keep the system breakpoint.
3. If a live call rejects the top-level field (verify with one call), fall back to block level: add the same optional field to `anthContentBlock` and, in `toAnthMessages`, set it on the last block of the last message whose type is not `thinking`. R5's `MarshalJSON` must emit it.

**Tests.** `internal/provider/anthropic/anthropic_test.go`: the captured request body has `cache_control` at the top level (or on the last block in the fallback). With a key, a two-turn run reports `cache_read_input_tokens > 0` on the second turn once the prefix exceeds the model's minimum; record the figure in the pull request.

**Note.** Anything that rewrites earlier messages (R6 pruning, R7 condensation) invalidates the cache from that point. That is expected once per rewrite; it must not happen every turn.

---

### R9 — Sandbox fails closed outside `--dev`; the unsandboxed shell gets a clean environment (T274)

**Problem.** `newSandboxFactory` (`cmd/nexusd/pipeline.go`) returns nil when `NEXUS_SANDBOX` is unset and also when it is set but Docker or OpenSandbox cannot be reached; it only logs a warning. With a nil factory, `builtin.Shell.Call` (`internal/tools/builtin/shell.go`) runs `/bin/sh -c` on the host as the `nexusd` user. `exec.Cmd.Env` is nil there, so the command inherits the process environment: the model provider key, database URLs and key file paths. This applies outside `--dev` too, although every other security-critical setting fails closed there.

**Change.**
1. `newSandboxFactory(workspaceRoot string, dev bool) (func(uuid.UUID) tools.SandboxExec, error)`:
   - `dev == true`: today's behaviour, unchanged.
   - `dev == false`: `NEXUS_SANDBOX` must be `docker` or `opensandbox`. Unset or any other value returns an error that names the variable and the two accepted values. A Docker connection error, or `opensandbox` without `NEXUS_OPENSANDBOX_URL`, returns an error instead of a warning.
2. `newToolPipeline` passes `devMode` (the package-level flag in `cmd/nexusd/main.go`) and returns the error; `serve` already fails on it.
3. `internal/tools/builtin/shell.go`, unsandboxed branch: set `cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + cmd.Dir, "LANG=C.UTF-8"}` (when `cmd.Dir` is empty use `os.TempDir()`).
4. Apply the same explicit environment in `internal/hooks/exechook/handler.go` only if its config has no way to pass variables; if it has one, pass exactly those.
5. Update `.env.example` (the `NEXUS_SANDBOX` entry) and `docs/agentic-capabilities.md`, which describes `NEXUS_SANDBOX`.

**Tests.** `cmd/nexusd`: `newSandboxFactory` with `dev=false` and the variable unset returns an error; with `dev=true` returns `(nil, nil)`. `internal/tools/builtin/shell_test.go`: `t.Setenv("NEXUS_TEST_SECRET", "leak")`, run `env` through `Shell{}.Call` with no sandbox, and require the output not to contain `leak`.

**Check.** `deploy/docker-compose.yml`'s app profile runs `--dev`, so it is unaffected. CI's `images` job must still pass.

---

### R10 — Egress client: check redirects and the address actually dialled (T275)

**Problem.** `builtin.WebFetch.Call` (`internal/tools/builtin/web_fetch.go`) resolves the host and checks the addresses, then calls `client.Do`, which resolves again and follows redirects. Two bypasses: an allow-listed host that redirects to an internal address or to a host not on the allowlist, and a DNS name that answers a public address to the check and a private one to the connection. `connector_fetch.go` and `internal/hooks/httphook/handler.go` have the same shape.

**Change.**
1. Add `internal/tools/builtin/egress.go` with `func newEgressClient(allowed []string, allowPrivate bool, timeout time.Duration) *http.Client`:
   - Transport: clone `http.DefaultTransport`, set `Proxy = nil`, and set `DialContext` to a `net.Dialer{Timeout: 10 * time.Second, Control: ...}` whose `Control(network, address string, _ syscall.RawConn) error` splits `address`, parses the IP and returns an error when `!allowPrivate` and the IP is not public. `Control` runs after resolution on the address being connected to, which closes the rebinding gap.
   - `CheckRedirect`: fail after 5 hops; fail unless the scheme is `http` or `https`; fail unless `hostAllowed(allowed, req.URL.Hostname())`.
2. Replace `isPrivateIP`'s list with: `ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()` plus the blocks `100.64.0.0/10`, `198.18.0.0/15`, `240.0.0.0/4` and `64:ff9b::/96`.
3. `WebFetch.Call` and `ConnectorFetch` use `newEgressClient` when their `Client` field is nil. Keep the existing resolve-and-check step: it gives a clearer error.
4. Apply the same dialer and redirect check in `internal/hooks/httphook/handler.go` (it is a different package; repeat the helper there, as the existing comment explains for `privateBlocks`).

**Tests.** `internal/tools/builtin/web_fetch_test.go` with `httptest`: an allow-listed server that redirects to a host not on the allowlist returns an error result; a client from `newEgressClient(..., false, ...)` refuses to connect to the test server's loopback address with an error that mentions the address; with `allowPrivate=true` the same request succeeds; six chained redirects fail.

---

### R11 — `/metrics` behind a token and cached; admin DSN required outside `--dev` (T276)

**Problem.**
- `GET /metrics` is mounted without authentication (`newMux` in `cmd/nexusd/surfaces_wiring.go`). It returns tenant ids, per-tenant spend and tool names, and each scrape runs several queries per tenant and opens a new superuser pool (`tenancypg.Directory.ListTenantIDs` in `internal/tenancy/postgres/directory.go`). Anyone who can reach the port can read it and can load the database with it.
- `adminDSN()` (`cmd/nexusd/background.go`) falls back to the built-in local default. `loadServerConfig` (`cmd/nexusd/config.go`) does not require `NEXUS_ADMIN_DATABASE_URL`, so a deployment that forgets it starts, and the tenant sweeps and orphan recovery fail quietly against `localhost`.

**Change.**
1. `cmd/nexusd/config.go`: add `AdminDatabaseURL` to `serverConfig`; outside dev `require("NEXUS_ADMIN_DATABASE_URL")`; in dev keep `adminDSN()`'s fallback. Add `MetricsToken`: outside dev `require("NEXUS_METRICS_TOKEN")`; in dev optional.
2. `cmd/nexusd/http.go`: add `requireBearer(token string, next http.Handler) http.Handler` comparing with `subtle.ConstantTimeCompare` and answering 401; when `token == ""` (dev only) it passes through. Wrap `handleMetrics` with it in `newMux`.
3. Cache the rendered response for 15 seconds: render into a `bytes.Buffer`, keep `{at time.Time; body []byte}` under a mutex, and serve the cached body when it is younger than that.
4. `api/openapi.yaml`: `/metrics` currently says `security: []` and "Unauthenticated, like the probes". Add a second security scheme for a static bearer token (it is not the tenant JWT the REST routes take), use it on `/metrics` and fix the description; run `make generate`. Read `cmd/nexusd/api_contract_test.go` and update whatever it asserts about `/metrics` being a probe. Update the `/metrics` request in `bruno/`, `docs/observability.md`, and the Prometheus scrape config under `deploy/observability/` (send the token).

**Tests.** `cmd/nexusd`: `/metrics` without a token answers 401 and with it 200; two requests inside the cache window call the reports once (count calls on a fake `reporting.Reports`); `loadServerConfig(false)` lists both new variables when they are missing.

**Note.** Removing the superuser credential from the running server altogether is spec 004 T033. Do not attempt it here.

---

### R12 — Verify bearer tokens with a public key file (T277)

**Problem.** `openInfra` (`cmd/nexusd/infra.go`) loads the Ed25519 **private** signing key and derives the public key from it. The server only verifies tokens, so it should not hold the key that mints them: anyone who reads that file can issue a token for any tenant.

**Change.**
1. `internal/authn/authn.go`: add `LoadPublicKey(r io.Reader) (ed25519.PublicKey, error)` reading exactly `ed25519.PublicKeySize` raw bytes, like `LoadPrivateKey`.
2. `cmd/nexusd/config.go`: add `AuthnPublicKeyPath` (`NEXUS_AUTHN_PUBLIC_KEY_PATH`). Outside dev require it and stop requiring `NEXUS_AUTHN_SIGNING_KEY_PATH` for `serve`. In dev, when it is unset, keep deriving the public key from the dev signing key.
3. `cmd/nexusd/infra.go`: build the verifier from the public key file when the path is set.
4. `cmd/nexusd/cli_token.go`: add `--public-key-out <path>` that writes the public half (mode 0644) so an operator can produce the file. The `token` command keeps reading the private key.
5. `.env.example`, `docs/go-live.md`.

**Tests.** `internal/authn/authn_test.go`: a token issued with a private key verifies with a verifier built from `LoadPublicKey` of the exported public half; a short file is an error. `cmd/nexusd`: config test for the new requirement.

**Out of scope.** A real identity provider (OIDC, key rotation, revocation, roles) is a new feature and starts with `/speckit-specify`.

---

### R13 — Redis connection with authentication and TLS (T278)

**Problem.** `openInfra` builds `redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})`: no password and no TLS can be configured. Redis carries the job queue, the session locks, the cost counters and the live content previews, which are plaintext (`docs/architecture.md`, "Where the code differs from the docs", row 9).

**Change.** Add `NEXUS_REDIS_URL` (`redis://` or `rediss://`, with user and password), parsed with `redis.ParseURL`. Put the choice in one helper in `cmd/nexusd`, `newRedisClient() (*redis.Client, error)`: the URL wins when set, otherwise `NEXUS_REDIS_ADDR` as today. Use it in `openInfra` and in `cmd/nexusd/cli_ingest.go`. Outside dev, `loadServerConfig` requires one of the two. Document it in `.env.example`.

**Tests.** `cmd/nexusd`: `newRedisClient` with a `rediss://user:pw@host:6380/0` URL yields options with TLS config set and the password; with only the address yields today's options; a malformed URL is an error.

---

### R14 — Queue: extend the lease while a job runs; a stale job is a no-op; the breaker recovers (T279)

**Problem (read, not reproduced).**
1. `Streams.Lease` (`internal/queue/redis/streams.go`) reclaims any entry idle longer than `leaseFor` with `XAutoClaim`. `WorkerConfig.LeaseFor` defaults to 2 minutes (`internal/queue/worker.go`) and nothing refreshes an entry's idle time while its job runs. A run longer than 2 minutes is therefore reclaimed by another worker, which finds the session lock held, calls `Fail` (retiring the original entry and re-adding the job 200 ms later) and repeats this on every poll for the rest of the run. When the first worker finishes, its `Complete` can fail with "no such leased job" (the copy's `Fail` deleted the claim record), and the copy then runs.
2. The copy calls `runctl.Control.Resume` on a session that is already terminal, suspended or awaiting input. `Resume` returns an error wrapping `runctl.ErrSessionState`; `queueRunner.Run` (`cmd/nexusd/background.go`) publishes that error to the session's event stream and returns it, so it is retried and then dead-lettered.
3. Each `Worker` creates its own `BreakerRegistry` (`withDefaults`), so breaker state is not shared between workers. A breaker that opens never closes: `Open()` stays true until `RecordSuccess`, and an open breaker never runs the job again. The registry map also grows by one entry per session for the life of the process.

**Change.**
1. `internal/queue/types.go`: add an optional interface (a port that does not implement it keeps working):
   ```go
   // LeaseExtender is an optional part of a Port. Extend tells the queue a
   // leased job is still being worked, so the lease is not treated as abandoned.
   type LeaseExtender interface {
       Extend(ctx context.Context, jobID uuid.UUID) error
   }
   ```
2. `internal/queue/redis/streams.go`: implement `Extend`: look the claim up with `claimedEntry`, then `XClaimJustID` with `Stream: streamKey, Group: groupName, Consumer: job.LeaseOwner, MinIdle: 0, Messages: []string{entryID}`. Claiming an entry for its current consumer resets its idle time; `JUSTID` leaves the delivery count alone.
3. `internal/queue/worker.go`: pass the job id to `holdLock`; on each tick, after a successful `Renew`, call `Extend` when `w.cfg.Port` implements `LeaseExtender`, logging a warning on error. `RenewEvery` (10s by default) is already well under `LeaseFor`.
4. `cmd/nexusd/background.go`, `queueRunner.Run`: when the error satisfies `errors.Is(err, runctl.ErrSessionState)`, log at info level with the job id and kind, do **not** publish it, and return nil so the job completes. A job for a session that no longer needs driving has nothing to do.
5. `internal/reliability/classifier.go`: give `CircuitBreaker` a cool-down. Record when it opened; `Open()` returns false once the cool-down (default 5 minutes, a field on `BreakerRegistry` passed to each breaker, with a `now func() time.Time` seam for tests) has passed, and leaves `consecutive` at `threshold-1` so one more identical failure re-opens it. Add `BreakerRegistry.Forget(key string)`.
6. `cmd/nexusd/background.go`, `startQueueWorkers`: create one `reliability.NewBreakerRegistry(3)` and pass it to every worker through `WorkerConfig.Breakers`. `internal/queue/worker.go`: after `breaker.RecordSuccess()` call `w.cfg.Breakers.Forget(job.SessionKey)`.

**Tests.**
- `internal/queue/worker_test.go`: a fake port that implements `LeaseExtender` and counts calls; a runner that blocks for three renew intervals; require at least two `Extend` calls. A port without the interface still works.
- `internal/queue/redis/streams_integration_test.go` (tag `integration`): lease a job with `leaseFor` 200 ms as consumer A; call `Extend` every 50 ms for 600 ms while consumer B polls `Lease` and must get nothing; stop extending and B gets the job within 400 ms.
- `internal/reliability/classifier_test.go`: open, advance the fake clock past the cool-down, `Open()` is false, one identical failure opens it again; `Forget` removes the entry.
- `cmd/nexusd`: a small pure helper (for example `staleJob(err error) bool`) with a table test, used by `queueRunner.Run`.

---

### R15 — Periodic orphan recovery (T280)

**Problem.** `runctl.StartRecovery` (`internal/runctl/recovery.go`) sweeps for sessions left `running` with no worker at startup and once more after the lock TTL, then never again. If a queued job is lost while the processes stay up (Redis restarted without its append-only file, a job dead-lettered by mistake), the session stays `running` until some process restarts. The comment explains why it is not periodic: a session whose resume fails for good would be re-enqueued forever.

**Change.**
1. `internal/runctl/postgres/orphans.go`, `ListRunning`: exclude sessions that have a claim in the in-flight state (read `internal/runctl/postgres/claims.go` for the table and status names). Those need a person, not a retry.
2. `internal/runctl/recovery.go`: add an optional throttle to `RecoverOrphans`:
   ```go
   // Throttle limits how often one session is re-enqueued by recovery.
   type Throttle interface {
       Allow(ctx context.Context, sessionKey string) (bool, error)
   }
   ```
   A nil throttle allows everything (today's behaviour). When it returns false or an error, skip the session and count it in the log line.
3. `internal/queue/redis/lock.go`: add `RecoveryThrottle` with `Allow` implemented as `SET nexus:recovery:<sessionKey> 1 NX EX 900`, so one session is re-enqueued by recovery at most once in 15 minutes.
4. `cmd/nexusd/background.go`: keep the startup sweep and the recheck, and add a periodic sweep with `startPeriodic` every 5 minutes that calls `listOrphans` and `runctl.RecoverOrphans(ctx, "periodic", ...)` with the throttle. Its stop function joins the others in `startBackground`.

**Tests.** `internal/runctl/recovery_test.go`: with a throttle that allows once, two sweeps over the same orphan enqueue one job; a nil throttle enqueues two; a held lock still skips. An integration test for `ListRunning` excluding a session with an in-flight claim.

---

### R16 — Drain in-flight runs on shutdown; recover a panic in a job (T281)

**Problem.**
- `startQueueWorkers` derives the workers' context from the process's signal context, so SIGTERM cancels every in-flight run at once, mid-turn. Principle VIII says a deploy must not cut a running agent over mid-task. The runs do resume later, but a tool call that was executing is recorded as interrupted and may need a person to resolve its claim.
- `Worker.pollOnce` calls `Runner.Run` with no `recover`. A panic anywhere in the kernel, a provider adapter or run control ends the whole process and every other run with it. (A panic inside a tool call is already recovered in `internal/tools/pipeline_helpers.go`.)

**Change.**
1. `internal/queue/worker.go`: add `JobContext context.Context` to `WorkerConfig` (nil means "use the context passed to `Run`", which is today's behaviour). In `pollOnce`, keep using the `Run` context for `Lease`, and use the job context for everything after a job is leased: admission, the lock, `holdLock`, the run, `Complete` and `Fail`.
2. `internal/queue/worker.go`: wrap the `Runner.Run` call in a helper that recovers a panic, logs it with `debug.Stack()`, and returns `&reliability.PermanentError{Err: fmt.Errorf("job panicked: %v", r)}`.
3. `cmd/nexusd/background.go`, `startQueueWorkers`: create `leaseCtx` from the incoming context and `jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))`; pass `JobContext: jobCtx`; run each worker with `leaseCtx`. The returned stop function waits for the workers for up to `NEXUS_SHUTDOWN_GRACE` (duration, default 25s), then calls `cancelJobs()` and waits for them to return, then closes the admin pool.
4. `.env.example` and `docs/go-live.md`: the orchestrator's termination grace period must exceed the HTTP drain (30s) plus `NEXUS_SHUTDOWN_GRACE`.

**Tests.** `internal/queue/worker_test.go`: a runner that records whether its context was cancelled and blocks until released; cancel the lease context and require the runner's context to stay live, then cancel the job context and require it to end. A runner that panics leads to `Port.Fail` with `permanent == true`, and the worker leases the next job afterwards. The package's `goleak` check must pass.

---

### R17 — Size a cost reservation from the prompt (T282)

**Problem.** `Gate` reserves a fixed worst case of `MaxInputTokenEstimate` (8,000) input tokens and `MaxOutputTokenEstimate` (4,096) output tokens for every call (`internal/cost/gate.go`, `gate_helpers.go`), whatever the prompt holds. An agent turn with a long history can send far more than 8,000 input tokens, so a ceiling can be overshot by a large factor in one call, and a call whose usage is never reported is charged far too little.

**Change.**
1. `internal/cost/gate.go`: add `InputTokenEstimate int64` to `ReserveRequest` (zero means "use the configured estimate").
2. `internal/cost/gate_helpers.go`: in the worst-case computation use `max(g.cfg.MaxInputTokenEstimate, req.InputTokenEstimate)`. Integer arithmetic only.
3. `internal/promptctx`: add `EstimateTokens(system string, catalog []provider.ToolSchema, transcript []provider.Message) int64`: total bytes of the system prompt, each schema's name, description and input schema, and each block's text and input, divided by 3 (a deliberately high estimate; this codebase has no tokenizer).
4. `kernel/turns.go`: compute the estimate from `st.Transcript` before the turn's `Reserve` call and pass it. Do the same for the condenser's `Reserve` in `kernel/metering.go`, using the covered messages.

**Tests.** `internal/cost/gate_test.go`: a request with `InputTokenEstimate` of 200,000 reserves more than the default, and a session ceiling below that amount refuses it. `internal/promptctx`: `EstimateTokens` grows with each part. `internal/cost/money_notfloat_test.go` must still pass.

---

### R18 — Test the real kernel and catalog against the provider's wire rules (T283)

**Problem.** R1, R2 and R5 were invisible because nothing exercises the production path against the provider's rules: the scripted provider accepts internal names, adapter tests use hand-built prompts, and the live evals use their own toy tools. `provider.ValidateTranscript` covers pairing only.

**Change.**
1. `internal/provider/anthropic`: add an exported, test-oriented `ValidateRequest(body []byte) error` that decodes a request body and enforces the documented rules: every tool name matches `^[a-zA-Z0-9_-]{1,128}$`; the first message has role `user`; every `tool_use` in an assistant message is answered by a `tool_result` in the next message and nowhere else; every `thinking` block has both a `thinking` and a `signature` field; every `tool_use` has an `input` object; at most four `cache_control` breakpoints.
2. One integration-tagged test that uses the real resident catalog. `newToolPipeline` is in package `main`, which `tests/integration` cannot import, so put the test in `cmd/nexusd` (file name ending `_integration_test.go`, build tag `integration`); if that needs too much wiring, build the catalog in `tests/integration` from the descriptors of the `internal/tools/builtin` tools instead. The test points the Anthropic adapter's `BaseURL` at an `httptest.Server` whose handler runs `ValidateRequest` on every body and replies with a scripted SSE stream containing a thinking block and two tool calls, and drives a full run through the kernel, including one resume. The test fails on the first invalid request.
3. `evals/live_corpus.go` (tag `liveeval`): add at least two cases that use schemas from the resident catalog, so the live track covers the names and descriptions production sends. Follow `.claude/rules/evals.md`.

**Done when.** The integration test fails on `main` before R1 and R5 and passes after them.

## Already tracked elsewhere (do not duplicate)

| Finding | Tracked as |
|---|---|
| A suspension's approval row is written in a separate transaction from its event | spec 001 T261; `docs/architecture.md` row 11 |
| The running server holds the administrative database credential | spec 004 T033; `docs/architecture.md` row 5 |
| A new connection to `signerd` is dialled for every receipt, with no deadline | spec 004 T034 (add a per-call deadline when doing it) |
| The tool calls of one turn run one after another | spec 004 T032 |
| Pruning and condensation are off by default | spec 004 T035 (needs R6 and R7 first) |
| Audit anchors live in the same database as the chain | spec 003 T006 |
| Delegated children and team members run on detached goroutines | spec 001 T264 |
| Live previews cross Redis as plaintext | `docs/architecture.md` row 9 (R13 reduces the exposure) |
| The router's choice does not select the model | `docs/architecture.md` row 6 |

## Needs an owner's decision before anyone implements it

- **Erasure and `payload_digest`.** The digest is an unsalted SHA-256 of the plaintext and survives crypto-shredding (`migrations/0003_events.sql`, `crypto.Digest`). A short payload can be confirmed by hashing guesses. A keyed digest changes the audit chain's inputs; this belongs in spec 003.
- **The `nexus_app` password** is written in `migrations/0000_app_role.sql`. Decide how a deployment sets it (an `ALTER ROLE` step in the go-live checklist, or a migration that reads a setting).
- **Model routing table** (`internal/provider/router.go`) and `defaultAnthropicModel` name previous-generation models. Changing them is a model change and must clear the live eval.
- **A permission denial ends the run** (`kernel/turns.go`, the `PermissionDenied` branch). Many agent runtimes return the denial to the model as a tool result so it can try another way. Changing this is a policy decision under Principle V.
- **Default worker count** is 2 runs per process (`queueWorkerCount`), with a ceiling of 64. Each worker is held for a whole run, most of it waiting on the model. Measure with `tests/integration/load_bench_test.go` before changing it.
- **Rehydration cost.** Every resume decrypts the session's whole history, and the checkpoint written after each job (`SaveCheckpoint`) is never read. Either use it or stop writing it.
- **The `events` table** is not partitioned and has two identical indexes on `(session_id, seq)` (the `UNIQUE` constraint and `events_session_seq_idx`).
- **The web app** (`web/src`, about 2,500 lines) has no tests.

## What was checked and what was not

| Claim | Basis |
|---|---|
| Build, vet and unit tests pass at `7b94aaa` | run |
| R1: tool names are rejected by the provider API | the code was read; the name rule was read from the provider's "Define tools" documentation on 2026-11-13; no live call was made |
| R2 to R17 | the code was read; none was reproduced by a test or a live call |
| R5 and R8's top-level cache field | inferred from the API's documented shapes; each item says to verify with one live call |
| Integration tests, lint, the eval gate | not run for this review |
