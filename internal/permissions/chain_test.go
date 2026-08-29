package permissions

import (
	"context"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
)

// countingModel proxies to an underlying verdict while counting how many
// times the safety model leg was actually invoked — the side channel
// chain_test.go uses to prove layer 6 ran (or didn't) without inspecting
// Chain internals.
type countingModel struct {
	calls   int32
	verdict safety.Verdict
}

func (m *countingModel) Classify(context.Context, string, string) (safety.Verdict, string, error) {
	atomic.AddInt32(&m.calls, 1)
	return m.verdict, "counting model", nil
}

func (m *countingModel) count() int { return int(atomic.LoadInt32(&m.calls)) }

// alwaysDeferSafety is a Chain config helper: no rules, a model leg that
// always defers, so layer 6 never independently objects — used by every
// cross-product case below that wants to isolate ONE other layer's effect.
func alwaysDeferSafety() *safety.Classifier {
	return safety.NewClassifier(nil, &countingModel{verdict: safety.VerdictDefer}, 0)
}

func passthroughRequest() Request {
	return Request{
		ToolID:      "platform/shell@v1",
		Namespace:   "platform",
		EffectClass: EffectClassMutating,
		Taint:       Taint{}, // no legs — keeps layer 7 out of the way unless a case configures otherwise
		Input:       `{"cmd":"echo hi"}`,
		Autonomy:    Pin(AutonomyAutonomous), // defers on every effect class — keeps layer 3 out of the way unless a case configures otherwise
		HookOutcome: LayerOutcome{Decision: Defer},
		Gate2:       LayerOutcome{Decision: Defer},
	}
}

// TestChainCrossProduct is README task 3.6's own acceptance line: a
// table-driven test over the layer's possible outcomes. Each case isolates
// exactly one layer's opinion against an otherwise all-Defer request, so the
// expected Decision/Layer pins down that layer's placement in the total
// order directly.
func TestChainCrossProduct(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*ChainConfig, *Request)
		wantDecide Decision
		wantLayer  Layer
	}{
		{
			name:       "nothing objects: fallback allow",
			mutate:     func(*ChainConfig, *Request) {},
			wantDecide: Allow,
			wantLayer:  LayerFallbackAllow,
		},
		{
			name: "layer 1 deny rule",
			mutate: func(cfg *ChainConfig, _ *Request) {
				cfg.DenyRules = []DenyRule{{Name: "no-shell", Tool: "platform/shell@v1", Reason: "blocked for this test"}}
			},
			wantDecide: Deny,
			wantLayer:  LayerDenyRules,
		},
		{
			name: "layer 2 hook denies",
			mutate: func(_ *ChainConfig, req *Request) {
				req.HookOutcome = LayerOutcome{Decision: Deny, Reason: "hook says no"}
			},
			wantDecide: Deny,
			wantLayer:  LayerPreToolUseHooks,
		},
		{
			name: "layer 2 hook asks, nothing else objects",
			mutate: func(_ *ChainConfig, req *Request) {
				req.HookOutcome = LayerOutcome{Decision: Ask, Reason: "hook wants confirmation"}
			},
			wantDecide: Ask,
			wantLayer:  LayerPreToolUseHooks,
		},
		{
			name: "layer 3 autonomy denies at read_only for a mutating effect",
			mutate: func(_ *ChainConfig, req *Request) {
				req.Autonomy = Pin(AutonomyReadOnly)
			},
			wantDecide: Deny,
			wantLayer:  LayerAutonomy,
		},
		{
			name: "layer 3 autonomy asks at supervised for a mutating effect",
			mutate: func(_ *ChainConfig, req *Request) {
				req.Autonomy = Pin(AutonomySupervised)
			},
			wantDecide: Ask,
			wantLayer:  LayerAutonomy,
		},
		{
			name: "layer 5 gate2 denies",
			mutate: func(_ *ChainConfig, req *Request) {
				req.Gate2 = LayerOutcome{Decision: Deny, Reason: "tool's own policy refuses"}
			},
			wantDecide: Deny,
			wantLayer:  LayerGate2Capability,
		},
		{
			name: "layer 5 gate2 asks",
			mutate: func(_ *ChainConfig, req *Request) {
				req.Gate2 = LayerOutcome{Decision: Ask, Reason: "tool's own policy wants confirmation"}
			},
			wantDecide: Ask,
			wantLayer:  LayerGate2Capability,
		},
		{
			name: "layer 6 safety denies on a rule match",
			mutate: func(cfg *ChainConfig, req *Request) {
				cfg.Safety = safety.NewClassifier(safety.DefaultRules(), &countingModel{verdict: safety.VerdictDefer}, 0)
				req.Input = `{"cmd":"DROP TABLE users;"}`
			},
			wantDecide: Deny,
			wantLayer:  LayerGate3Safety,
		},
		{
			name: "layer 6 safety asks on a rule match",
			mutate: func(cfg *ChainConfig, req *Request) {
				cfg.Safety = safety.NewClassifier(safety.DefaultRules(), &countingModel{verdict: safety.VerdictDefer}, 0)
				req.Input = `{"cmd":"sudo rm x"}`
			},
			wantDecide: Ask,
			wantLayer:  LayerGate3Safety,
		},
		{
			name: "layer 7 rule of two asks on a third leg",
			mutate: func(_ *ChainConfig, req *Request) {
				req.Taint = Taint{ReturnsUntrusted: true, ReadsPrivateData: true, MutatesExternal: true}
			},
			wantDecide: Ask,
			wantLayer:  LayerRuleOfTwo,
		},
		{
			name: "layer 8 approval policy asks",
			mutate: func(cfg *ChainConfig, _ *Request) {
				cfg.Approval = ApprovalPolicy{RequireAskFor: map[EffectClass]AskKind{EffectClassMutating: AskMultiParty}}
			},
			wantDecide: Ask,
			wantLayer:  LayerApprovalPolicy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ChainConfig{
				Profiles: ProfileSet{Profiles: []ToolProfile{NewToolProfile("default", 1, "platform/shell@v1")}},
				Safety:   alwaysDeferSafety(),
			}
			req := passthroughRequest()
			tc.mutate(&cfg, &req)

			c := NewChain(cfg)
			got, err := c.Resolve(context.Background(), req)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Resolution.Decision != tc.wantDecide || got.Resolution.Layer != tc.wantLayer {
				t.Fatalf("Resolve() = {%v at %v} (%s), want {%v at %v}",
					got.Resolution.Decision, got.Resolution.Layer, got.Resolution.Reason, tc.wantDecide, tc.wantLayer)
			}
		})
	}
}

func TestChain_Layer4NotAMemberDenies(t *testing.T) {
	cfg := ChainConfig{Profiles: ProfileSet{}, Safety: alwaysDeferSafety()} // no profiles bound at all
	req := passthroughRequest()
	c := NewChain(cfg)
	got, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Resolution.Decision != Deny || got.Resolution.Layer != LayerGate1Profile {
		t.Fatalf("Resolve() = {%v at %v}, want {Deny at LayerGate1Profile}", got.Resolution.Decision, got.Resolution.Layer)
	}
}

// TestChain_DenyIsFinal_NeverReachesSafety proves DENY short-circuits: a
// deny rule at layer 1 must mean the safety model leg (layer 6) is never
// even invoked.
func TestChain_DenyIsFinal_NeverReachesSafety(t *testing.T) {
	model := &countingModel{verdict: safety.VerdictDefer}
	cfg := ChainConfig{
		DenyRules: []DenyRule{{Name: "block-all", Tool: "*", Reason: "test"}},
		Profiles:  ProfileSet{Profiles: []ToolProfile{NewToolProfile("default", 1, "platform/shell@v1")}},
		Safety:    safety.NewClassifier(nil, model, 0), // no rules: the model leg is the ONLY way layer 6 could produce an opinion
	}
	req := passthroughRequest()
	c := NewChain(cfg)

	got, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Resolution.Decision != Deny || got.Resolution.Layer != LayerDenyRules {
		t.Fatalf("Resolve() = {%v at %v}, want {Deny at LayerDenyRules}", got.Resolution.Decision, got.Resolution.Layer)
	}
	if model.count() != 0 {
		t.Fatalf("safety model leg was called %d times, want 0 — a layer-1 deny must short-circuit before layer 6", model.count())
	}
}

// TestChain_StandingScopeCannotSkipLayer6Or7 is README.md §5's Phase 3
// acceptance test, made concrete: even though a standing scope is known in
// advance to satisfy the ask this call will raise, layer 6 (safety) still
// actually runs — proven by the model-leg call counter — and layer 7 (Rule
// of Two) still actually commits its taint-state effect, proven by the
// returned TaintState showing the call's legs engaged, despite the final
// answer being Allow.
func TestChain_StandingScopeCannotSkipLayer6Or7(t *testing.T) {
	model := &countingModel{verdict: safety.VerdictDefer}
	cfg := ChainConfig{
		Profiles:       ProfileSet{Profiles: []ToolProfile{NewToolProfile("default", 1, "platform/shell@v1")}},
		Approval:       ApprovalPolicy{RequireAskFor: map[EffectClass]AskKind{EffectClassMutating: AskOnce}}, // layer 8 raises the ask
		StandingScopes: []StandingScope{{Name: "ops-preauth", ToolPattern: "platform/shell@v1"}},
		Safety:         safety.NewClassifier(nil, model, 0), // no rules: any opinion at all proves the model leg ran
	}
	req := passthroughRequest()
	req.Taint = Taint{MutatesExternal: true} // one Rule-of-Two leg this call should engage

	c := NewChain(cfg)
	got, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Resolution.Decision != Allow || got.Resolution.Layer != LayerStandingScope {
		t.Fatalf("Resolve() = {%v at %v}, want {Allow at LayerStandingScope}", got.Resolution.Decision, got.Resolution.Layer)
	}
	if model.count() != 1 {
		t.Fatalf("safety model leg was called %d times, want exactly 1 — layer 6 must run even though a standing scope will satisfy the eventual ask", model.count())
	}
	if !got.TaintState.Engaged[LegExternalEffect] {
		t.Fatal("TaintState does not show the external-effect leg engaged — layer 7 must actually run (and its effect must persist) even when a standing scope later satisfies the ask")
	}
}

func TestChain_AskWithoutStandingScopeStaysAsk(t *testing.T) {
	cfg := ChainConfig{
		Profiles: ProfileSet{Profiles: []ToolProfile{NewToolProfile("default", 1, "platform/shell@v1")}},
		Approval: ApprovalPolicy{RequireAskFor: map[EffectClass]AskKind{EffectClassMutating: AskOnce}},
		Safety:   alwaysDeferSafety(),
		// no StandingScopes
	}
	req := passthroughRequest()
	c := NewChain(cfg)
	got, err := c.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Resolution.Decision != Ask {
		t.Fatalf("Resolve() Decision = %v, want Ask (no standing scope should satisfy it)", got.Resolution.Decision)
	}
}

func TestChain_RejectsALayerThatResolvesAllow(t *testing.T) {
	cfg := ChainConfig{
		Profiles: ProfileSet{Profiles: []ToolProfile{NewToolProfile("default", 1, "platform/shell@v1")}},
		Safety:   alwaysDeferSafety(),
	}
	req := passthroughRequest()
	req.Gate2 = LayerOutcome{Decision: Allow, Reason: "a buggy Tool.CheckPermissions"}
	c := NewChain(cfg)
	if _, err := c.Resolve(context.Background(), req); err == nil {
		t.Fatal("Resolve() = nil error, want an error: a layer function must never resolve Allow")
	}
}

func TestChain_DenyRulePatternMatchesInput(t *testing.T) {
	cfg := ChainConfig{
		DenyRules: []DenyRule{{Name: "no-rm", Tool: "platform/shell@v1", Pattern: regexp.MustCompile(`rm -rf`), Reason: "test"}},
		Profiles:  ProfileSet{Profiles: []ToolProfile{NewToolProfile("default", 1, "platform/shell@v1")}},
		Safety:    alwaysDeferSafety(),
	}
	c := NewChain(cfg)

	denied := passthroughRequest()
	denied.Input = `{"cmd":"rm -rf build/"}`
	got, err := c.Resolve(context.Background(), denied)
	if err != nil || got.Resolution.Decision != Deny {
		t.Fatalf("Resolve(rm -rf) = %v, %v, want Deny", got.Resolution.Decision, err)
	}

	allowed := passthroughRequest()
	allowed.Input = `{"cmd":"echo hi"}`
	allowed.Autonomy = Pin(AutonomyAutonomous)
	got, err = c.Resolve(context.Background(), allowed)
	if err != nil || got.Resolution.Decision != Allow {
		t.Fatalf("Resolve(echo hi) = %v, %v, want Allow", got.Resolution.Decision, err)
	}
}
