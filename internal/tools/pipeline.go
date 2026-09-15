package tools

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
)

// Invocation is what the pipeline receives for one tool_use. It is kept
// independent of any specific caller's own request shape (a kernel
// ToolUseRequest, a future eval harness's own shape, ...) so this package
// has no reason to import a caller's package — kernel/tools_adapter.go is
// where kernel.ToolUseRequest gets translated into this.
type Invocation struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	ToolName      string // the qualified {ns}/{name}@{ver} string form
	Input         json.RawMessage
	AutonomyLevel string // "read_only" | "supervised" | "autonomous"; only consulted the first time a session is seen — see stateFor's doc comment
}

// ExecuteResult is the pipeline's answer for one Invocation.
type ExecuteResult struct {
	Output           json.RawMessage
	IsError          bool
	Reason           string
	PermissionDenied bool
	AwaitingApproval bool
	AskKind          string

	// ApprovalMismatch marks a result ExecuteApproved produced because the
	// digest it recomputed at resume time didn't match the digest a human
	// approved (README task 5.7) — refused, never a silent re-request.
	ApprovalMismatch bool

	// CanonicalDigest is set only alongside AwaitingApproval: the digest
	// (steps 5/8's CanonicalDigest, over {tool_id, input} as it stood at
	// the moment the chain asked) internal/oversight.Approvals.Create binds
	// an approval to, and ExecuteApproved later re-verifies at resume time.
	CanonicalDigest []byte

	// EffectClass is set only alongside AwaitingApproval: the tool's own
	// descriptor.EffectClass, folded into the ContextPackage an approver
	// sees (internal/oversight.ContextPackage) — carried here rather than
	// re-resolved later because Pipeline's own Registry/Manifest are
	// unexported, and this is the one place that already has the
	// descriptor in hand.
	EffectClass string

	// AwaitingDelegation/ChildSessionID mirror Result.AwaitingChildSessionID
	// one layer up (README task 8.10) — finishCall's own short-circuit,
	// translated by kernel/tools_adapter.go into kernel.ToolResult exactly
	// like AwaitingApproval already is.
	AwaitingDelegation bool
	ChildSessionID     uuid.UUID
}

func errorResult(reason string) ExecuteResult { return ExecuteResult{IsError: true, Reason: reason} }

// PipelineConfig is everything one Pipeline needs at construction time —
// the resident catalog, the permission chain's tenant/session-independent
// config, the hook chain's static configuration, and where oversized
// results spill to. Per-session state (autonomy, Rule-of-Two taint) is
// tracked internally, keyed by SessionID, not part of this config.
type PipelineConfig struct {
	Registry    *Registry
	Manifest    Manifest
	Chain       *permissions.Chain
	Hooks       *hooks.Dispatcher
	HookConfigs []hooks.Config
	Blobs       BlobStore

	// DerivedArtifacts, if set, tracks each blob spill (README task 5.4) so
	// internal/crypto/shred.go's erasure and reconciliation can find and
	// hard-delete it. Nil is valid — spills simply go untracked, the
	// pre-Phase-5 behavior every existing caller and test still gets.
	DerivedArtifacts DerivedArtifactRecorder

	// WorkspaceRoot is the local directory each session's filesystem-
	// touching builtin tools (file_read/file_write/file_search) are scoped
	// under, one subdirectory per SessionID — and, once SandboxFactory is
	// set, the same directory a session's sandbox bind-mounts at
	// /workspace (internal/sandbox.Config.WorkspaceDir), so both paths
	// agree on what "the session's files" means.
	WorkspaceRoot string

	// SandboxFactory, if set, returns the SandboxExec (README task 5.12)
	// platform/shell runs through for sessionID, one per invocation — cheap
	// enough to call unconditionally (internal/sandbox.SessionSandbox is
	// just a struct binding a Docker client + a per-session Config; no
	// container exists until Exec is actually called). Nil is valid and
	// leaves RunContext.Sandbox unset — the pre-Phase-5 unsandboxed
	// fallback WorkspaceRoot's own doc comment used to name as the honest
	// interim.
	SandboxFactory func(sessionID uuid.UUID) SandboxExec

	// Claims, if set, is the write-ahead idempotency hook README task 6.6
	// names — wrapped around Tool.Call for every non-read-only effect class
	// in finishCall. Nil is valid and simply skips write-ahead tracking, the
	// pre-Phase-6 behavior every existing test still gets.
	Claims Claims

	// Dynamic, if set, is consulted at step 1 only when the static Manifest
	// doesn't resolve ref (README task 11.1) — Phase 11's per-tenant MCP
	// tools can never live in the single process-wide Manifest/Registry
	// pair (they vary per tenant; the resident catalog does not), so a miss
	// against the pinned manifest falls through here instead of failing
	// immediately. Nil is valid and simply skips this fallback, the
	// pre-Phase-11 behavior every existing test still gets.
	Dynamic DynamicResolver
}

// DynamicResolver resolves a ToolRef the static Manifest doesn't know about,
// scoped to the calling tenant and session — internal/surfaces/mcp.Resolver
// is the one implementation (README task 11.1). sessionID is threaded
// through (not just tenantID) because a remote MCP server admitted with
// auth_kind='oauth_connector' authenticates as the CALLING SESSION's own
// user (internal/connectors.Vault is keyed by (tenant, user, provider)),
// mirroring platform/connector_fetch's own re-derive-the-user-from-the-
// session discipline one layer up. ok=false (with err=nil) means "not
// resolvable for this tenant," which Execute/ExecuteApproved turn into the
// same unknown_tool error a static-manifest miss already produces; there is
// no separate "found but not admitted" signal because a tool this resolver
// won't hand back is, from the pipeline's point of view, not resolvable at
// all — indistinguishable from never having existed.
type DynamicResolver interface {
	Resolve(ctx context.Context, tenantID, sessionID uuid.UUID, ref ToolRef) (Tool, bool, error)
}

// sessionState is the per-session facilities the pipeline can't share
// across sessions: the pinned autonomy ratchet and the accumulated
// Rule-of-Two taint projection.
type sessionState struct {
	autonomy *permissions.Autonomy

	// taintMu guards taintState across the whole read-resolve-write
	// sequence in Execute's step 9 — NOT just the final write. Two
	// concurrent calls in the same session (even against a
	// concurrency-safe tool, which never takes serialLock at all) must
	// never both read the same taintState, resolve independently, and race
	// to write back: that would silently let one call's Rule-of-Two
	// engagement clobber the other's instead of accumulating.
	taintMu    sync.Mutex
	taintState permissions.TaintState

	serialLock sync.Mutex // step 12's in-process serial slot for a non-concurrency-safe tool
}

// Pipeline is the single execution path (README task 3.4, pattern 16):
// construct once, share across every run this process serves, and call
// Execute per tool_use. It implements the shape kernel/tools_adapter.go
// wraps into a kernel.ToolExecutor — this package itself never imports
// kernel (kernel is the one allowed to depend on tools, never the reverse;
// kernel/types.go's own doc comment names the allowed direction).
type Pipeline struct {
	cfg PipelineConfig

	mu       sync.Mutex
	sessions map[uuid.UUID]*sessionState
}

func NewPipeline(cfg PipelineConfig) *Pipeline {
	return &Pipeline{cfg: cfg, sessions: map[uuid.UUID]*sessionState{}}
}

// stateFor returns (creating on first use) the per-session state for
// sessionID, pinning autonomy from autonomyLevel the first time this
// session is seen. Every subsequent call ignores autonomyLevel — Pin is a
// one-time thing (internal/permissions.Autonomy's own doc comment); a
// session's autonomy only ever moves via Tighten from here on.
func (p *Pipeline) stateFor(sessionID uuid.UUID, autonomyLevel string) *sessionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	if !ok {
		level, err := permissions.ParseAutonomyLevel(autonomyLevel)
		if err != nil {
			level = permissions.AutonomyReadOnly // fail closed on an unrecognized level
		}
		s = &sessionState{autonomy: permissions.Pin(level)}
		p.sessions[sessionID] = s
	}
	return s
}

// TaintStateFor snapshots sessionID's current Rule-of-Two engaged legs — used
// by internal/delegate at TWO moments (README task 8.11): reading the
// PARENT's state at spawn time (to seed the child's own copy-at-spawn), and
// reading the CHILD's state at return time (to fold into the parent).
// Honest scope note, matching this codebase's own convention for a
// documented gap rather than a silent one: this is Pipeline's own
// process-lifetime cache, not a replay of a durable taint_transition event
// stream — migrations/0002_sessions.sql's own taint_state column is marked
// PROJECTION but nothing in this codebase (pre- or post-Phase-8) actually
// writes it yet, so a worker restart mid-run loses it exactly the way it
// already loses every other piece of Pipeline.sessionState. Wiring a real
// durable projection is future work, not a Phase 8 regression.
func (p *Pipeline) TaintStateFor(sessionID uuid.UUID) [3]bool {
	state := p.stateFor(sessionID, "")
	state.taintMu.Lock()
	defer state.taintMu.Unlock()
	return state.taintState.Engaged
}

// FoldTaint folds engaged (a child session's own event-derived Rule-of-Two
// legs) into sessionID's running TaintState (README task 8.11 — the
// taint-ascend rule: "a summary never clears the untrusted leg"). Callers
// (internal/delegate's return-time resolution) call this exactly once per
// resolved delegation, BEFORE any further tool_use in sessionID is
// dispatched — engagedCount only ever grows, matching layer 7's own
// ResolveRuleOfTwo semantics, so a session already at two legs that folds in
// a third is exactly as constrained afterward as if it had engaged that
// third leg itself. Creates sessionID's state (pinned to AutonomyReadOnly,
// the same fail-closed default stateFor uses for an unrecognized level) if
// this is the first thing this process has ever seen for it — a delegation
// can resolve after every OTHER call this process ever routed through
// Execute for the parent, so sessionID is not guaranteed to already have an
// entry.
func (p *Pipeline) FoldTaint(sessionID uuid.UUID, engaged [3]bool) {
	state := p.stateFor(sessionID, "")
	state.taintMu.Lock()
	defer state.taintMu.Unlock()
	for i, e := range engaged {
		if e {
			state.taintState.Engaged[i] = true
		}
	}
}

// ResetTurn forwards to the hook dispatcher's per-turn cap reset
// (internal/hooks task 3.11) — the kernel loop calls this once per turn,
// before dispatching any tool_use in that turn.
func (p *Pipeline) ResetTurn() {
	if p.cfg.Hooks != nil {
		p.cfg.Hooks.ResetTurn()
	}
}
