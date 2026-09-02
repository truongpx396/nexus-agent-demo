package plan

import "testing"

func simplePlan() Plan {
	return Plan{
		StartStep:    "a",
		AllowedTools: []string{"platform/file_read@v1"},
		Steps: []Step{
			{ID: "a", Kind: StepAgent, Agent: &AgentStepConfig{Input: "go"}, Transitions: []Transition{{To: "b"}}},
			{ID: "b", Kind: StepCondition, Condition: &ConditionConfig{}},
		},
	}
}

func TestValidate_AcceptsASimplePlan(t *testing.T) {
	if err := Validate(simplePlan()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsMissingStartStep(t *testing.T) {
	p := simplePlan()
	p.StartStep = "nowhere"
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a start_step that names no declared step")
	}
}

func TestValidate_RejectsDuplicateStepID(t *testing.T) {
	p := simplePlan()
	p.Steps = append(p.Steps, Step{ID: "a", Kind: StepCondition, Condition: &ConditionConfig{}})
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a duplicate step id")
	}
}

func TestValidate_RejectsTransitionToUndeclaredStep(t *testing.T) {
	p := simplePlan()
	p.Steps[1].Transitions = []Transition{{To: "nowhere"}}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a transition to an undeclared step")
	}
}

func TestValidate_RejectsUnreachableStep(t *testing.T) {
	p := simplePlan()
	p.Steps = append(p.Steps, Step{ID: "orphan", Kind: StepCondition, Condition: &ConditionConfig{}})
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a step unreachable from start_step")
	}
}

func TestValidate_RejectsUnconditionalTransitionNotLast(t *testing.T) {
	p := simplePlan()
	cond := &Predicate{Op: OpEq, Field: "x", Value: BoolValue(true)}
	p.Steps[0].Transitions = []Transition{{To: "b"}, {To: "b", When: cond}}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error when the unconditional (nil when) transition isn't listed last")
	}
}

func TestValidate_RejectsCycleWithNoLoopStep(t *testing.T) {
	p := Plan{
		StartStep:    "a",
		AllowedTools: nil,
		Steps: []Step{
			{ID: "a", Kind: StepCondition, Condition: &ConditionConfig{}, Transitions: []Transition{{To: "b"}}},
			{ID: "b", Kind: StepCondition, Condition: &ConditionConfig{}, Transitions: []Transition{{To: "a"}}},
		},
	}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for an unbounded cycle with no Loop-kind step in it")
	}
}

func TestValidate_AcceptsCycleThroughABoundedLoopStep(t *testing.T) {
	p := Plan{
		StartStep: "a",
		Steps: []Step{
			{ID: "a", Kind: StepLoop, Loop: &LoopConfig{MaxIterations: 3}, Transitions: []Transition{{To: "b"}}},
			{ID: "b", Kind: StepCondition, Condition: &ConditionConfig{}, Transitions: []Transition{{To: "a"}}},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsLoopStepWithoutMaxIterations(t *testing.T) {
	p := simplePlan()
	p.Steps[1] = Step{ID: "b", Kind: StepLoop, Loop: &LoopConfig{MaxIterations: 0}}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a loop step with max_iterations <= 0")
	}
}

func TestValidate_RejectsScopeGrantOutsideAllowedTools(t *testing.T) {
	p := Plan{
		StartStep:    "fan",
		AllowedTools: []string{"platform/file_read@v1"},
		CostEnvelope: CostEnvelope{Ceiling: "1.00"},
		Steps: []Step{
			{ID: "fan", Kind: StepDelegateFanout, DelegateFanout: &DelegateFanoutConfig{
				AgentID: "worker", Task: "go", ChildCount: 2, PerChildCeiling: "0.10",
				ScopeGrant: []string{"platform/shell@v1"}, // not in AllowedTools
			}},
		},
	}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a scope_grant naming a tool outside the plan's own allowed_tools")
	}
}

func TestValidate_RequiresCostEnvelopeForDelegateFanout(t *testing.T) {
	p := Plan{
		StartStep:    "fan",
		AllowedTools: []string{"platform/file_read@v1"},
		Steps: []Step{
			{ID: "fan", Kind: StepDelegateFanout, DelegateFanout: &DelegateFanoutConfig{
				AgentID: "worker", Task: "go", ChildCount: 2, PerChildCeiling: "0.10",
				ScopeGrant: []string{"platform/file_read@v1"},
			}},
		},
	}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error: a plan with a delegate_fanout step must declare cost_envelope.ceiling")
	}
}

func TestValidate_RejectsEmptyPreauth(t *testing.T) {
	p := simplePlan()
	p.Steps[1] = Step{ID: "b", Kind: StepPreauth, Preauth: &PreauthConfig{}}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error: an empty/unbounded preauth must fail validation (README task 8.7)")
	}
}

func TestValidate_AcceptsBoundedPreauth(t *testing.T) {
	p := simplePlan()
	p.Steps[1] = Step{ID: "b", Kind: StepPreauth, Preauth: &PreauthConfig{Entries: []PreauthEntry{
		{ToolID: "platform/shell@v1", Digest: []byte{1, 2, 3}},
	}}}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsPredicateNestedTooDeep(t *testing.T) {
	p := simplePlan()
	pred := Predicate{Op: OpEq, Field: "x", Value: BoolValue(true)}
	for i := 0; i < maxPredicateDepth+2; i++ {
		pred = Predicate{Op: OpAnd, And: []Predicate{pred}}
	}
	p.Steps[0].Transitions = []Transition{{To: "b", When: &pred}}
	if err := Validate(p); err == nil {
		t.Fatalf("want an error for a predicate nested past maxPredicateDepth")
	}
}
