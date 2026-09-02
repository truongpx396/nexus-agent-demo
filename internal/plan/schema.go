// Package plan is the declarative orchestration plane (README.md §8, Phase
// 8, tasks 8.1-8.7): a Plan is DATA — a closed JSON shape with a closed
// predicate AST (predicate.go) — never a string expression language, which
// is what makes "zero-token routing" a property of the format itself
// (task 8.6's own test asserts no Provider.Stream call happens while
// evaluating a transition) rather than a convention a future step author
// could accidentally break.
package plan

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// StepKind is the closed set of step shapes a Plan may contain (README task
// 8.1).
type StepKind string

const (
	StepAgent          StepKind = "agent"
	StepDelegateFanout StepKind = "delegate_fanout"
	StepApprovalGate   StepKind = "approval_gate"
	StepPreauth        StepKind = "preauth"
	StepInputRequest   StepKind = "input_request"
	StepCondition      StepKind = "condition"
	StepLoop           StepKind = "loop"
)

// Transition is one outgoing edge from a step. When is nil for the
// unconditional/default edge — a step may have at most one nil-When
// transition, and it must be listed LAST (validate.go enforces this so
// evaluation order is unambiguous without needing a priority field).
type Transition struct {
	To   string     `json:"to"`
	When *Predicate `json:"when,omitempty"`
}

// AgentStepConfig runs one ordinary kernel turn sequence: Input is appended
// as a fresh user message (mirroring Kernel.Run's own opening message), and
// the run continues until its OWN terminal state (or an ordinary tool
// approval suspends it — Executor.Resume's own doc comment covers that
// case). OutputVar names the Context key the step's final assistant content
// is bound to, for later transitions/steps to read.
type AgentStepConfig struct {
	Input     string `json:"input"`
	OutputVar string `json:"output_var,omitempty"`
}

// DelegateFanoutConfig spawns ChildCount children, each granted ScopeGrant
// and given Task, drawing from ONE envelope sized PerChildCeiling*ChildCount
// (README task 8.13). CompletionVar, if set, is bound to the count of
// children that returned successfully once the whole cohort resolves.
type DelegateFanoutConfig struct {
	AgentID         string   `json:"agent_id"`
	Task            string   `json:"task"`
	ScopeGrant      []string `json:"scope_grant"`
	ChildCount      int      `json:"child_count"`
	PerChildCeiling string   `json:"per_child_ceiling"` // decimal, cost.ParseDecimal
	CompletionVar   string   `json:"completion_var,omitempty"`
}

// ApprovalGateConfig suspends the plan's own driving session for a human
// decision, reusing internal/oversight.Approvals directly (README task
// 8.10's "no second suspension mechanism" — this is the SAME primitive,
// invoked from a different call site than kernel/loop.go's own tool-use-
// triggered suspend). ResultVar records "granted"|"denied".
type ApprovalGateConfig struct {
	Question  string `json:"question"`
	ResultVar string `json:"result_var,omitempty"`
}

// PreauthConfig enumerates a BOUNDED set of {tool_id, input digest} pairs
// for one human decision (README task 8.7) — a preauth admitting anything
// outside this enumeration fails validation (validate.go).
type PreauthConfig struct {
	Entries []PreauthEntry `json:"entries"`
}

type PreauthEntry struct {
	ToolID string `json:"tool_id"`
	Digest []byte `json:"digest"`
}

// InputRequestConfig pulls a structured answer from a human — zero
// authorization value (mirrors internal/oversight/input.go's own task 5.9
// rule), reused here for a plan-level question.
type InputRequestConfig struct {
	Question  string          `json:"question"`
	Schema    json.RawMessage `json:"schema,omitempty"`
	ResultVar string          `json:"result_var,omitempty"`
}

// ConditionConfig carries no behavior of its own — it exists purely so a
// branch point in the plan has a step to attach Transitions to, evaluated
// at zero token cost like every other transition.
type ConditionConfig struct{}

// LoopConfig bounds a cycle in the transition graph (README task 8.3's
// "bounded loops"): a step of kind Loop is the only step a cycle may pass
// through, and MaxIterations must be positive. CounterVar, if set, is bound
// to the number of times this step has been entered so far THIS plan run —
// predicates can read it to decide when to break out.
type LoopConfig struct {
	MaxIterations int    `json:"max_iterations"`
	CounterVar    string `json:"counter_var,omitempty"`
}

// Step is one node in the plan's transition graph.
type Step struct {
	ID   string   `json:"id"`
	Kind StepKind `json:"kind"`

	Agent          *AgentStepConfig      `json:"agent,omitempty"`
	DelegateFanout *DelegateFanoutConfig `json:"delegate_fanout,omitempty"`
	ApprovalGate   *ApprovalGateConfig   `json:"approval_gate,omitempty"`
	Preauth        *PreauthConfig        `json:"preauth,omitempty"`
	InputRequest   *InputRequestConfig   `json:"input_request,omitempty"`
	Condition      *ConditionConfig      `json:"condition,omitempty"`
	Loop           *LoopConfig           `json:"loop,omitempty"`

	Transitions []Transition `json:"transitions"`
}

// CostEnvelope bounds what the WHOLE plan run may spend — a session-scoped
// ceiling (internal/cost.CreateBudget, BudgetScopeSession) created against
// the plan's own driving session when the plan starts.
type CostEnvelope struct {
	Ceiling string `json:"ceiling"` // decimal, cost.ParseDecimal; empty = unenforced
}

// Plan is the full declarative shape (README task 8.1): Steps[] plus a
// CostEnvelope. AllowedTools is the plan's own declared tool universe —
// every DelegateFanout.ScopeGrant entry must be a subset of it
// (validate.go's own scope-subset proof).
type Plan struct {
	PlanID       uuid.UUID    `json:"plan_id"`
	TenantID     uuid.UUID    `json:"tenant_id"`
	Version      int          `json:"version"`
	Name         string       `json:"name"`
	StartStep    string       `json:"start_step"`
	Steps        []Step       `json:"steps"`
	AllowedTools []string     `json:"allowed_tools"`
	CostEnvelope CostEnvelope `json:"cost_envelope"`

	// AgentVersion/RouteModelID are pinned at Enable (README task 8.4), not
	// set by the plan's own author — zero values here until then.
	AgentVersion int    `json:"agent_version,omitempty"`
	RouteModelID string `json:"route_model_id,omitempty"`
}

// StepByID looks up one step, or ok=false if none has that id.
func (p Plan) StepByID(id string) (Step, bool) {
	for _, s := range p.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

// parseCeiling is CostEnvelope/PerChildCeiling's shared decimal parse —
// empty string means "unenforced," matching internal/cost's own "no budget
// row = nothing to enforce" convention (internal/cost/gate.go's Reserve).
func parseCeiling(s string) (cost.Money, bool, error) {
	if s == "" {
		return cost.Money{}, false, nil
	}
	m, err := cost.ParseDecimal(s, cost.DefaultCurrency)
	return m, true, err
}
