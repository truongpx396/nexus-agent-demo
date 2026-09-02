package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// TaintFolder is the two operations README task 8.11 needs from
// internal/tools.Pipeline: read a session's current Rule-of-Two engaged
// legs, and fold another session's legs into one. Declared here (rather
// than depending on *tools.Pipeline directly) so this package names only
// the two methods it actually calls — the same granularity idiom
// internal/tools/builtin's own SkillResolver/SkillEvents already use.
type TaintFolder interface {
	TaintStateFor(sessionID uuid.UUID) [3]bool
	FoldTaint(sessionID uuid.UUID, engaged [3]bool)
}

// Locker is the session-key serial lock (internal/queue.SessionLock,
// README task 6.2) — acquired around resuming the PARENT, so two children
// of the same fan-out (or a child's return racing an operator's cancel)
// never both drive the parent's kernel run concurrently. Nil is valid (every
// unit test, and any single-worker deployment) and simply skips locking.
type Locker interface {
	Acquire(ctx context.Context, sessionKey string) (token string, ok bool, err error)
	Release(ctx context.Context, sessionKey, token string) error
}

// FanoutResolver is internal/plan.Executor's own structural seam: a
// delegate_fanout child's completion routes here instead of through the
// ordinary kernel.ResumeDelegation path (README task 8.13) — a fan-out
// step isn't a live kernel turn loop suspended on one tool_use, it's the
// plan executor's own step waiting on a whole cohort. Nil is valid (every
// ad-hoc, non-fanout delegation).
type FanoutResolver interface {
	ResolveFanout(ctx context.Context, tenantID, fanoutID uuid.UUID) error
}

// SpawnRequest is everything Spawn needs to create a child session and
// start it running.
type SpawnRequest struct {
	TenantID        uuid.UUID
	ParentSessionID uuid.UUID
	AgentID         string
	Task            string
	ScopeGrant      []string
	ReturnSchema    json.RawMessage
	// FanoutID/EnvelopeID are set only for a delegate_fanout plan step's
	// cohort (README task 8.13); nil for an ordinary ad hoc `delegate` call.
	FanoutID   *uuid.UUID
	EnvelopeID *uuid.UUID
}

// Config is Delegations' construction-time collaborators beyond deps —
// everything a spawned child's own kernel.Run call needs, mirroring
// internal/oversight.Resumer's own System/Catalog/MaxTurns fields exactly,
// for the identical reason: no per-tenant/per-agent config store exists yet
// (Phase 7's internal/config), so one process-wide system prompt and
// resident catalog cover a delegated child exactly as they cover a root run.
type Config struct {
	Kernel      *kernel.Kernel
	Pipeline    TaintFolder
	Lock        Locker         // optional
	Fanout      FanoutResolver // optional
	System      string
	Catalog     []provider.ToolSchema
	LoadedTools []string
	MaxTurns    int
}

// Wire attaches Spawn/Resolve's runtime collaborators — split from
// NewDelegations so a test can construct a bare *Delegations against only
// deps (store.go's own scanning tests) without also standing up a Kernel.
func (d *Delegations) Wire(cfg Config) *Delegations {
	d.cfg = cfg
	return d
}

// Spawn creates a child session (README task 8.9: depth = parent.Depth+1,
// root_session_id inherited, delegation_role="delegate"), copies the
// parent's current taint state into it at creation (task 8.11's "starts as
// a copy of the parent's at spawn"), records a pending delegations row, and
// starts the child's own kernel.Run in the background — Spawn itself never
// blocks on the child's completion; the CALLER (platform/delegate's own
// Call, via kernel's AwaitingDelegation branch) is what suspends for that.
//
// Every bound/scope check (README task 8.9's "CheckPermissions re-derives
// scope_grant as a provable subset... never trusted from the input") has
// ALREADY run by the time Spawn is called — Spawn trusts req exactly the
// way Pipeline.finishCall trusts any other tool's Call after the permission
// chain already resolved Allow.
func (d *Delegations) Spawn(ctx context.Context, req SpawnRequest) (uuid.UUID, error) {
	if d.cfg.Kernel == nil {
		return uuid.Nil, fmt.Errorf("delegate: Spawn called before Wire")
	}

	var parent store.Session
	var childID = uuid.New()
	var dek crypto.DEK
	err := d.Store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		parent, err = store.GetSession(ctx, tx, req.ParentSessionID)
		if err != nil {
			return err
		}
		dek, err = d.Keys.NewDEK(ctx, tx, req.TenantID)
		if err != nil {
			return err
		}
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID:      childID,
			SessionKey:     childID.String(),
			TenantID:       req.TenantID,
			SurfaceID:      "delegate",
			UserID:         parent.UserID,
			AgentID:        uuid.Nil,
			AgentVersion:   1,
			HarnessDigest:  parent.HarnessDigest,
			DataLabel:      parent.DataLabel,
			RouteModelID:   parent.RouteModelID,
			RouteReason:    parent.RouteReason,
			AutonomyLevel:  parent.AutonomyLevel,
			RootSessionID:  parent.RootSessionID,
			Depth:          parent.Depth + 1,
			DelegationRole: "delegate",
		}); err != nil {
			return err
		}
		if _, err := create(ctx, tx, Delegation{
			TenantID: req.TenantID, ParentSessionID: req.ParentSessionID, ChildSessionID: childID,
			FanoutID: req.FanoutID, AgentID: req.AgentID, Task: req.Task,
			ScopeGrant: req.ScopeGrant, ReturnSchema: req.ReturnSchema,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("delegate: spawn: %w", err)
	}

	// Copy-at-spawn (task 8.11): the child inherits the trust context it
	// was spawned from, in-process, immediately — before its first tool
	// call can possibly run.
	if d.cfg.Pipeline != nil {
		d.cfg.Pipeline.FoldTaint(childID, d.cfg.Pipeline.TaintStateFor(req.ParentSessionID))
	}

	childState := &kernel.RunState{TenantID: req.TenantID, SessionID: childID, Seal: sealFuncFor(dek, req.TenantID, childID)}
	childCfg := kernel.RunConfig{
		System: d.cfg.System, Catalog: d.cfg.Catalog, LoadedTools: d.cfg.LoadedTools,
		ModelID: parent.RouteModelID, MaxTurns: d.cfg.MaxTurns,
		Input: req.Task, AutonomyLevel: parent.AutonomyLevel,
	}

	// A delegate_fanout child (README task 8.13) runs under a CLONED
	// Kernel whose Budget is this cohort's shared EnvelopeBudgetGate instead
	// of the process's real internal/cost.Gate — every OTHER field (the
	// live Provider/Tools/Store/Receipts/OnSuspend/OnDelegate) stays
	// identical, so this is purely a budget-routing swap, not a different
	// execution path.
	childKernel := d.cfg.Kernel
	if req.EnvelopeID != nil {
		perCall, currency, err := envelopePerCallEstimate(ctx, d.Store, req.TenantID, *req.EnvelopeID)
		if err != nil {
			slog.Error("delegate: envelope lookup failed; child will run under the ordinary budget gate", "envelope_id", *req.EnvelopeID, "error", err)
		} else {
			clone := *d.cfg.Kernel
			clone.Budget = &EnvelopeBudgetGate{Store: d.Store, EnvelopeID: *req.EnvelopeID, TenantID: req.TenantID, PerCallEstimate: perCall, Currency: currency}
			childKernel = &clone
		}
	}

	go func() {
		bg := context.Background()
		for _, err := range childKernel.Run(bg, childState, childCfg) {
			if err != nil {
				slog.Error("delegate: child run errored", "child_session_id", childID, "error", err)
				return
			}
		}
		// The child's own Run generator stopping does not always mean it
		// reached a terminal state — it may have suspended on an ordinary
		// tool approval (kernel/loop.go's own suspendForApproval), in which
		// case OnChildTerminal below is a documented no-op (the child isn't
		// terminal yet) and resolution happens later, whenever THAT
		// approval is eventually granted/denied — see OnChildTerminal's own
		// doc comment for where that second call site lives.
		if err := d.OnChildTerminal(bg, req.TenantID, childID); err != nil {
			slog.Error("delegate: resolve after child run failed", "child_session_id", childID, "error", err)
		}
	}()

	return childID, nil
}

func sealFuncFor(dek crypto.DEK, tenantID, sessionID uuid.UUID) kernel.SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("delegate: seal child event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}
