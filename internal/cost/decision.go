package cost

import (
	"github.com/google/uuid"
)

// DecisionKind is Reserve's typed resolution — one of these fires on
// EVERY Reserve call, including an unenforced one (README task 4.6: "an
// unenforced ceiling is visibly distinct from a ceiling with room").
type DecisionKind string

const (
	// DecisionAllow: a ceiling exists and this reservation fit under it
	// with room to spare.
	DecisionAllow DecisionKind = "allow"
	// DecisionRefuseCeiling: a ceiling exists and this reservation would
	// have breached it (or the epoch-marked counter backing it was
	// unavailable — fail closed, README task 4.4's "unknown epoch = fail
	// closed", folded into the same public decision kind since a caller
	// must react identically either way: refuse).
	DecisionRefuseCeiling DecisionKind = "refuse_ceiling"
	// DecisionDegrade: this reservation fit, but crossed the soft
	// (pre-ceiling) threshold GateConfig.DegradeThresholdPercent
	// declares — a signal a future router/compaction step could act on to
	// downgrade rather than simply keep spending; Phase 4 emits the
	// signal, nothing downstream reacts to it yet.
	DecisionDegrade DecisionKind = "degrade"
	// DecisionSkip: no budget (neither session- nor tenant-scoped) was
	// ever configured for this call — nothing was reserved because there
	// was nothing to check against.
	DecisionSkip DecisionKind = "skip"
)

// Decision is one gate resolution: the kind, a human-readable reason, and
// which budget (if any) produced it — the shape Reserve's caller (kernel)
// both appends as a eventlog.EventBudgetDecision and this package durably
// records in budget_decisions.
type Decision struct {
	Kind     DecisionKind
	Reason   string
	BudgetID *uuid.UUID // the deciding budget, nil for DecisionSkip
	Reserved Money      // worst-case amount reserved; zero for skip/refuse
}
