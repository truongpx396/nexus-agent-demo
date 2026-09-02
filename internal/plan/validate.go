package plan

import (
	"fmt"
	"sort"
)

// maxPredicateDepth bounds how deeply And/Or may nest — a closed AST still
// needs SOME bound to stay a property a validator can check in bounded
// time; 8 is far past anything a hand-authored plan would ever need.
const maxPredicateDepth = 8

// Validate runs every check README task 8.3 names: schema, reachability,
// bounded loops, closed predicates, a scope-subset proof per step, and
// oversight completeness. It is pure — no I/O, no tenant lookup — callers
// (lifecycle.go's own Validate transition) are what persist the outcome.
func Validate(p Plan) error {
	if p.StartStep == "" {
		return fmt.Errorf("plan: start_step is required")
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("plan: at least one step is required")
	}

	seen := map[string]Step{}
	for _, s := range p.Steps {
		if s.ID == "" {
			return fmt.Errorf("plan: a step has an empty id")
		}
		if _, dup := seen[s.ID]; dup {
			return fmt.Errorf("plan: duplicate step id %q", s.ID)
		}
		seen[s.ID] = s
		if err := validateStepShape(s); err != nil {
			return fmt.Errorf("plan: step %q: %w", s.ID, err)
		}
	}
	if _, ok := seen[p.StartStep]; !ok {
		return fmt.Errorf("plan: start_step %q is not a declared step", p.StartStep)
	}

	for _, s := range p.Steps {
		if err := validateTransitions(s); err != nil {
			return fmt.Errorf("plan: step %q: %w", s.ID, err)
		}
		for _, t := range s.Transitions {
			if _, ok := seen[t.To]; !ok {
				return fmt.Errorf("plan: step %q transitions to undeclared step %q", s.ID, t.To)
			}
			if t.When != nil {
				if depth := predicateDepth(*t.When); depth > maxPredicateDepth {
					return fmt.Errorf("plan: step %q's transition to %q nests predicates %d deep (max %d)", s.ID, t.To, depth, maxPredicateDepth)
				}
			}
		}
	}

	if err := validateReachability(p, seen); err != nil {
		return err
	}
	if err := validateBoundedLoops(p, seen); err != nil {
		return err
	}
	if err := validateScopeSubset(p); err != nil {
		return err
	}
	if err := validateOversightCompleteness(p); err != nil {
		return err
	}
	if _, ok, err := parseCeiling(p.CostEnvelope.Ceiling); err != nil {
		return fmt.Errorf("plan: cost_envelope.ceiling: %w", err)
	} else if !ok && p.hasDelegateFanout() {
		return fmt.Errorf("plan: a plan with a delegate_fanout step must declare a cost_envelope.ceiling (README task 8.13 — reserved before the first child starts)")
	}
	return nil
}

func (p Plan) hasDelegateFanout() bool {
	for _, s := range p.Steps {
		if s.Kind == StepDelegateFanout {
			return true
		}
	}
	return false
}

func validateStepShape(s Step) error {
	present := 0
	check := func(ok bool) {
		if ok {
			present++
		}
	}
	check(s.Agent != nil)
	check(s.DelegateFanout != nil)
	check(s.ApprovalGate != nil)
	check(s.Preauth != nil)
	check(s.InputRequest != nil)
	check(s.Condition != nil)
	check(s.Loop != nil)
	if present != 1 {
		return fmt.Errorf("exactly one of the kind-specific configs must be set for kind %q (found %d)", s.Kind, present)
	}
	switch s.Kind {
	case StepAgent:
		if s.Agent == nil {
			return fmt.Errorf("kind %q requires an agent config", s.Kind)
		}
	case StepDelegateFanout:
		if s.DelegateFanout == nil {
			return fmt.Errorf("kind %q requires a delegate_fanout config", s.Kind)
		}
		if s.DelegateFanout.ChildCount <= 0 {
			return fmt.Errorf("delegate_fanout.child_count must be positive")
		}
		if len(s.DelegateFanout.ScopeGrant) == 0 {
			return fmt.Errorf("delegate_fanout.scope_grant must name at least one tool")
		}
		if _, _, err := parseCeiling(s.DelegateFanout.PerChildCeiling); err != nil {
			return fmt.Errorf("delegate_fanout.per_child_ceiling: %w", err)
		}
	case StepApprovalGate:
		if s.ApprovalGate == nil || s.ApprovalGate.Question == "" {
			return fmt.Errorf("kind %q requires a non-empty question", s.Kind)
		}
	case StepPreauth:
		if s.Preauth == nil || len(s.Preauth.Entries) == 0 {
			// README task 8.7: "a preauth admitting anything outside its
			// enumeration fails validation" — an EMPTY enumeration is the
			// degenerate case of "outside the enumeration is everything,"
			// so it fails here rather than silently admitting nothing.
			return fmt.Errorf("preauth must enumerate at least one {tool_id, digest} entry — an unbounded or empty preauth fails validation")
		}
	case StepInputRequest:
		if s.InputRequest == nil || s.InputRequest.Question == "" {
			return fmt.Errorf("kind %q requires a non-empty question", s.Kind)
		}
	case StepCondition:
		// no fields to check
	case StepLoop:
		if s.Loop == nil || s.Loop.MaxIterations <= 0 {
			return fmt.Errorf("loop steps must declare a positive max_iterations (README task 8.3: bounded loops)")
		}
	default:
		return fmt.Errorf("unrecognized step kind %q", s.Kind)
	}
	return nil
}

// validateTransitions enforces that at most one transition per step has a
// nil When (the default edge), and that it is listed last — evaluation
// order is unambiguous without a priority field.
func validateTransitions(s Step) error {
	for i, t := range s.Transitions {
		if t.When == nil && i != len(s.Transitions)-1 {
			return fmt.Errorf("the unconditional transition (nil when) must be listed last")
		}
	}
	return nil
}

func predicateDepth(p Predicate) int {
	switch p.Op { //nolint:exhaustive // every leaf op (eq/ne/lt/gt/in) shares the same depth-1 default; only and/or recurse
	case OpAnd, OpOr:
		max := 0
		for _, sub := range append(append([]Predicate{}, p.And...), p.Or...) {
			if d := predicateDepth(sub); d > max {
				max = d
			}
		}
		return max + 1
	default:
		return 1
	}
}

// validateReachability walks the transition graph from StartStep — every
// declared step must be reachable, or it's dead weight a plan author almost
// certainly didn't intend (README task 8.3).
func validateReachability(p Plan, seen map[string]Step) error {
	visited := map[string]bool{p.StartStep: true}
	queue := []string{p.StartStep}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, t := range seen[id].Transitions {
			if !visited[t.To] {
				visited[t.To] = true
				queue = append(queue, t.To)
			}
		}
	}
	var unreachable []string
	for id := range seen {
		if !visited[id] {
			unreachable = append(unreachable, id)
		}
	}
	if len(unreachable) > 0 {
		sort.Strings(unreachable)
		return fmt.Errorf("plan: unreachable step(s) from start_step %q: %v", p.StartStep, unreachable)
	}
	return nil
}

// validateBoundedLoops detects cycles in the transition graph and requires
// every cycle to pass through at least one StepLoop-kind step (which
// validateStepShape already forced to declare a positive max_iterations) —
// otherwise a plan could route around the zero-token property by simply
// never terminating (README task 8.3).
func validateBoundedLoops(p Plan, seen map[string]Step) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var walk func(id string, path []string) error
	walk = func(id string, path []string) error {
		color[id] = gray
		path = append(path, id)
		for _, t := range seen[id].Transitions {
			switch color[t.To] {
			case white:
				if err := walk(t.To, path); err != nil {
					return err
				}
			case gray:
				// Found a cycle back to t.To — the cycle is path[idx(t.To):] + t.To.
				idx := indexOf(path, t.To)
				cycle := append(append([]string{}, path[idx:]...), t.To)
				if !cycleHasLoopStep(cycle, seen) {
					return fmt.Errorf("plan: cycle %v has no bounded loop step (README task 8.3 — a cycle must pass through a Loop-kind step with a positive max_iterations)", cycle)
				}
			case black:
				// a DAG edge into an already-fully-explored subtree — fine
			}
		}
		color[id] = black
		return nil
	}
	if err := walk(p.StartStep, nil); err != nil {
		return err
	}
	return nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func cycleHasLoopStep(cycle []string, seen map[string]Step) bool {
	for _, id := range cycle {
		if seen[id].Kind == StepLoop {
			return true
		}
	}
	return false
}

// validateScopeSubset is task 8.3's "scope-subset proof per step": every
// delegate_fanout's scope_grant must be a subset of the plan's own declared
// AllowedTools — the plan cannot hand a child more than it itself claims to
// need, mirroring platform/delegate's own CheckPermissions doing the same
// proof at runtime against the admitted catalog (README task 8.9).
func validateScopeSubset(p Plan) error {
	allowed := map[string]bool{}
	for _, t := range p.AllowedTools {
		allowed[t] = true
	}
	for _, s := range p.Steps {
		if s.DelegateFanout == nil {
			continue
		}
		for _, g := range s.DelegateFanout.ScopeGrant {
			if !allowed[g] {
				return fmt.Errorf("plan: step %q's scope_grant names %q, which is not in the plan's own allowed_tools", s.ID, g)
			}
		}
	}
	return nil
}

// validateOversightCompleteness is task 8.3's "a plan that routes around
// its own tenant's approval policy fails validation." This schema has no
// field anywhere capable of suppressing an approval outside a bounded
// preauth enumeration (validateStepShape already refuses an empty/missing
// one) — so completeness holds by the schema's own closure, and this
// function's job is to make that an explicit, checked property rather than
// an implicit one: every preauth step's enumeration must be finite (it
// already is, being a []PreauthEntry) and non-empty.
func validateOversightCompleteness(p Plan) error {
	for _, s := range p.Steps {
		if s.Kind != StepPreauth {
			continue
		}
		for i, e := range s.Preauth.Entries {
			if e.ToolID == "" || len(e.Digest) == 0 {
				return fmt.Errorf("plan: step %q preauth entry %d is missing tool_id or digest — every entry must be a fully bound {tool_id, digest} pair, never a wildcard", s.ID, i)
			}
		}
	}
	return nil
}
