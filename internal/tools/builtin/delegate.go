package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

var delegateRef = tools.ToolRef{Namespace: "platform", Name: "delegate", Version: "v1"}

// Bound thresholds mirror internal/delegate.MaxDepth/MaxConcurrent/MaxPerRun
// (README task 8.12) — duplicated as plain values, not imported, so this
// tool's own CheckPermissions logic stays unit-testable with plain Go fakes
// and no live Postgres, the same decoupling *ActivateSkill already gets
// from SkillResolver/SkillEvents rather than importing internal/skills'
// concrete Catalog type.
const (
	maxDelegationDepth       = 1
	maxConcurrentDelegations = 3
	maxDelegationsPerRun     = 16
)

// DelegationSpawner is the lookup Call needs — satisfied structurally by a
// small cmd/nexusd adapter over *internal/delegate.Delegations.Spawn.
type DelegationSpawner interface {
	Spawn(ctx context.Context, req SpawnRequest) (childSessionID uuid.UUID, err error)
}

// SpawnRequest is Delegate's own view of what Spawn needs — deliberately
// primitive-typed (no uuid.UUID beyond TenantID/ParentSessionID, no
// internal/delegate types) so this package never needs to import
// internal/delegate at all.
type SpawnRequest struct {
	TenantID        uuid.UUID
	ParentSessionID uuid.UUID
	AgentID         string
	Task            string
	ScopeGrant      []string
	ReturnSchema    json.RawMessage
}

// DelegationLedger is the bounds lookup CheckPermissions needs — satisfied
// structurally by *internal/delegate.Delegations (ParentContext/CountForRoot
// already have exactly this shape).
type DelegationLedger interface {
	ParentContext(ctx context.Context, tenantID, sessionID uuid.UUID) (depth int, rootSessionID uuid.UUID, err error)
	CountForRoot(ctx context.Context, tenantID, rootSessionID uuid.UUID) (open, total int, err error)
}

// Delegate implements platform/delegate(agent_id, task, scope_grant,
// return_schema): an ordinary Tool, no new kernel ABI (README task 8.9).
// CheckPermissions re-derives every bound (depth/concurrent/per_run) and
// re-verifies scope_grant against the admitted catalog itself — none of it
// is trusted from the input, mirroring how internal/tools/pipeline.go never
// trusts a hook's own claim about what it changed. Taint() intentionally
// returns the all-TRUE default: autonomy level and the Rule of Two gate a
// delegation call in addition to, not instead of, these bounds.
type Delegate struct {
	Spawner  DelegationSpawner
	Ledger   DelegationLedger
	Registry *tools.Registry
}

type delegateInput struct {
	AgentID      string          `json:"agent_id"`
	Task         string          `json:"task"`
	ScopeGrant   []string        `json:"scope_grant"`
	ReturnSchema json.RawMessage `json:"return_schema,omitempty"`
}

func (Delegate) ID() tools.ToolRef { return delegateRef }

func (Delegate) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          delegateRef,
		Description: "Delegates a bounded sub-task to a fresh child agent session, returning its result once it completes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string"},"task":{"type":"string"},"scope_grant":{"type":"array","items":{"type":"string"}},"return_schema":{"type":"object"}},"required":["agent_id","task","scope_grant"]}`),
		EffectClass: tools.EffectClassMutating,
	}
}

// Taint defaults every leg TRUE (tools.DefaultTaint) — the kernel ABI's own
// rule (README.md §4): an under-declared tool fails closed, and a
// delegation specifically must never be cheaper, taint-wise, than the
// riskiest thing it could plausibly cause the child to do.
func (Delegate) Taint() tools.Taint { return tools.DefaultTaint() }

// IsConcurrencySafe is false (the package default posture for a mutating
// tool) — spawning a child touches this session's own delegation bounds
// (open/total counts), which step 12's in-process serial slot protects from
// a same-session race exactly like any other non-concurrency-safe tool.
func (Delegate) IsConcurrencySafe(json.RawMessage) bool { return false }

func (d Delegate) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req delegateInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.AgentID == "" || req.Task == "" {
		return fmt.Errorf("agent_id and task are required")
	}
	if len(req.ScopeGrant) == 0 {
		return fmt.Errorf("scope_grant must name at least one tool ref")
	}
	return nil
}

// CheckPermissions is Gate 2 (README task 8.9): re-derive depth/concurrent/
// per_run against the CURRENT ledger state, and re-verify scope_grant is a
// provable subset of the tenant's own admitted catalog — never trusted from
// the request. Every failure here is Deny, never Ask: an over-bound or
// over-scoped delegation is not a judgment call a human approval should
// paper over, it's a structural refusal, the same posture Gate 1 (tool
// profile membership) already takes.
func (d Delegate) CheckPermissions(ctx context.Context, in json.RawMessage, rc tools.RunContext) tools.PermissionResult {
	var req delegateInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.PermissionResult{Decision: "deny", Reason: "invalid input: " + err.Error()}
	}
	if d.Ledger == nil {
		return tools.PermissionResult{Decision: "deny", Reason: "delegation ledger not wired (fail closed)"}
	}
	depth, root, err := d.Ledger.ParentContext(ctx, rc.TenantID, rc.SessionID)
	if err != nil {
		return tools.PermissionResult{Decision: "deny", Reason: "delegation bounds lookup failed (fail closed): " + err.Error()}
	}
	if depth+1 > maxDelegationDepth {
		return tools.PermissionResult{Decision: "deny", Reason: fmt.Sprintf("delegation depth bound exceeded: a session at depth %d may not delegate further", depth)}
	}
	open, total, err := d.Ledger.CountForRoot(ctx, rc.TenantID, root)
	if err != nil {
		return tools.PermissionResult{Decision: "deny", Reason: "delegation bounds lookup failed (fail closed): " + err.Error()}
	}
	if open >= maxConcurrentDelegations {
		return tools.PermissionResult{Decision: "deny", Reason: fmt.Sprintf("concurrent delegation bound exceeded: %d already open (max %d)", open, maxConcurrentDelegations)}
	}
	if total >= maxDelegationsPerRun {
		return tools.PermissionResult{Decision: "deny", Reason: fmt.Sprintf("per-run delegation bound exceeded: %d already created (max %d)", total, maxDelegationsPerRun)}
	}

	if d.Registry == nil {
		return tools.PermissionResult{Decision: "deny", Reason: "tool registry not wired (fail closed)"}
	}
	for _, s := range req.ScopeGrant {
		ref, err := tools.ParseToolRef(s)
		if err != nil {
			return tools.PermissionResult{Decision: "deny", Reason: fmt.Sprintf("scope_grant entry %q is not a valid tool ref: %v", s, err)}
		}
		if _, ok := d.Registry.Lookup(ref); !ok {
			return tools.PermissionResult{Decision: "deny", Reason: fmt.Sprintf("scope_grant names %q, which is not in the admitted catalog", s)}
		}
		status, _ := d.Registry.AdmissionStatus(ref)
		if status != tools.AdmissionClean {
			return tools.PermissionResult{Decision: "deny", Reason: fmt.Sprintf("scope_grant names %q, which is not admitted clean", s)}
		}
	}
	return tools.PermissionResult{Decision: "defer"}
}

// Call spawns the child (CheckPermissions has already re-verified every
// bound and scope entry by the time this runs) and returns immediately —
// the effect already started, asynchronously, which is exactly what
// AwaitingChildSessionID communicates to Pipeline.finishCall (README task
// 8.10).
func (d Delegate) Call(ctx context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req delegateInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	if d.Spawner == nil {
		return tools.Result{IsError: true, Reason: "delegation spawner not wired"}, nil
	}
	childID, err := d.Spawner.Spawn(ctx, SpawnRequest{
		TenantID: rc.TenantID, ParentSessionID: rc.SessionID,
		AgentID: req.AgentID, Task: req.Task, ScopeGrant: req.ScopeGrant, ReturnSchema: req.ReturnSchema,
	})
	if err != nil {
		return tools.Result{IsError: true, Reason: "spawn failed: " + err.Error()}, nil
	}
	out, err := json.Marshal(map[string]string{"child_session_id": childID.String(), "status": "delegated"})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out, AwaitingChildSessionID: &childID}, nil
}
