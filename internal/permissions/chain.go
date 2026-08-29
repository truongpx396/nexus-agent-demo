package permissions

import (
	"context"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
)

// ApprovalPolicy is layer 8: a per-effect-class tenant policy that can
// demand an ask even when nothing upstream did. A nil/empty map is AUTO for
// everything — the demo default, since Phase 3 ships no tenant config
// store (that's Phase 7's internal/config) to load one from.
type ApprovalPolicy struct {
	RequireAskFor map[EffectClass]AskKind
}

// Resolve is layer 8's evaluation.
func (p ApprovalPolicy) Resolve(effectClass EffectClass) LayerOutcome {
	if kind, ok := p.RequireAskFor[effectClass]; ok {
		return LayerOutcome{Decision: Ask, AskKind: kind, Reason: fmt.Sprintf("approval policy requires a %q approval for %s effects", kind, effectClass)}
	}
	return LayerOutcome{Decision: Defer, Reason: "approval policy: auto"}
}

// StandingScope is layer 9: a preauthorization that can SATISFY an
// outstanding Ask, never suppress one outright and never manufacture an
// Allow from nothing (README.md §4's chain table, row 9).
type StandingScope struct {
	Name        string
	ToolPattern string // same glob semantics as a DenyRule's Tool field
}

func (s StandingScope) covers(toolID, namespace string) bool {
	return matchGlob(s.ToolPattern, toolID, namespace)
}

// ChainConfig is everything one tenant/session binds the chain to. Every
// field is static, in-process config for Phase 3 — internal/config (Phase
// 7) is what will load these from tenant rows instead of a Go literal.
type ChainConfig struct {
	DenyRules      []DenyRule
	Profiles       ProfileSet
	Approval       ApprovalPolicy
	StandingScopes []StandingScope
	Safety         *safety.Classifier // defaults to safety.DefaultRules() with no model leg if nil
}

// Chain resolves one published total order (README task 3.6). Construct
// once per tenant/session config and reuse across every tool invocation in
// that session — resolve.go's Safety classifier carries its own circuit
// breaker state across calls, which only makes sense if the Chain is
// reused, not rebuilt per call.
type Chain struct {
	cfg ChainConfig
}

func NewChain(cfg ChainConfig) *Chain {
	if cfg.Safety == nil {
		cfg.Safety = safety.NewClassifier(safety.DefaultRules(), nil, 0)
	}
	return &Chain{cfg: cfg}
}

// Request is everything Resolve needs for one invocation. HookOutcome and
// Gate2 are precomputed by the caller (internal/tools/pipeline.go, which
// owns dispatching hooks and calling Tool.CheckPermissions) — this package
// only folds their answers into the order at the right position; it never
// dispatches a hook or calls into a Tool itself, keeping it a leaf with
// respect to both internal/hooks and internal/tools.
type Request struct {
	ToolID      string
	Namespace   string
	EffectClass EffectClass
	Taint       Taint
	Input       string // the invocation's raw input JSON — what layers 1 and 6 pattern-match against

	Autonomy    *Autonomy
	HookOutcome LayerOutcome // layer 2
	Gate2       LayerOutcome // layer 5
	TaintState  TaintState   // this session's Rule-of-Two state going in
}

// Result is Resolve's answer: the binding Resolution, plus the Rule-of-Two
// state advanced by this call (which the caller persists as a
// taint_transition regardless of the final Decision — ResolveRuleOfTwo's
// doc comment explains why an Ask still advances it).
type Result struct {
	Resolution Resolution
	TaintState TaintState
}

// guardNotAllow defends the one invariant no layer function may violate:
// layers 1-8 never resolve ALLOW. A hook or a Tool.CheckPermissions
// implementation is external code this package doesn't control, so this is
// a runtime check, not just a doc comment — see profile_test.go and
// chain_test.go for the case that actually exercises it.
func guardNotAllow(l Layer, o LayerOutcome) (LayerOutcome, error) {
	if o.Decision == Allow {
		return LayerOutcome{}, fmt.Errorf("permissions: layer %s resolved Allow, which is not a valid layer outcome", l)
	}
	return o, nil
}

// Resolve walks all 10 layers in the published order. A Deny at any layer
// is final and stops the walk immediately (there is no bypass). An Ask
// never stops the walk — layers 6 and 7 always run, and layer 9 is only
// ever checked once every other layer has spoken — so "a standing scope
// cannot cause layer 6 or 7 to be skipped" (README.md §5's Phase 3
// acceptance test) holds by construction, not by a special case.
func (c *Chain) Resolve(ctx context.Context, req Request) (Result, error) {
	accumulated := Resolution{Decision: Defer}

	fold := func(l Layer, raw LayerOutcome) (stop bool, err error) {
		o, err := guardNotAllow(l, raw)
		if err != nil {
			return false, err
		}
		switch o.Decision {
		case Deny:
			accumulated = Resolution{Decision: Deny, Layer: l, Reason: o.Reason}
			return true, nil
		case Ask:
			if accumulated.Decision != Ask {
				accumulated = Resolution{Decision: Ask, Layer: l, Reason: o.Reason, AskKind: o.AskKind}
			}
		case Defer:
			// no change
		case Allow:
			// unreachable: guardNotAllow above already turned this into an
			// error before o could ever carry Allow here. Listed explicitly
			// so this switch stays exhaustive over the Decision type.
		}
		return false, nil
	}

	// Layer 1: deny rules.
	if stop, err := fold(LayerDenyRules, ResolveDenyRules(c.cfg.DenyRules, req.ToolID, req.Namespace, req.Input)); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: req.TaintState}, err
	}

	// Layer 2: PreToolUse hooks (precomputed).
	if stop, err := fold(LayerPreToolUseHooks, req.HookOutcome); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: req.TaintState}, err
	}

	// Layer 3: autonomy.
	autonomyOutcome := LayerOutcome{Decision: Defer, Reason: "no autonomy pinned"}
	if req.Autonomy != nil {
		autonomyOutcome = req.Autonomy.Resolve(req.EffectClass)
	}
	if stop, err := fold(LayerAutonomy, autonomyOutcome); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: req.TaintState}, err
	}

	// Layer 4: Gate 1, tool profile membership.
	if stop, err := fold(LayerGate1Profile, c.cfg.Profiles.Resolve(req.ToolID)); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: req.TaintState}, err
	}

	// Layer 5: Gate 2, capability metadata (precomputed).
	if stop, err := fold(LayerGate2Capability, req.Gate2); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: req.TaintState}, err
	}

	// Layer 6: Gate 3, per-invocation safety — ALWAYS EVALUATED.
	sr := c.cfg.Safety.Classify(ctx, req.ToolID, req.Input)
	if stop, err := fold(LayerGate3Safety, LayerOutcome{Decision: Decision(sr.Verdict), Reason: sr.Reason}); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: req.TaintState}, err
	}

	// Layer 7: Rule of Two — ALWAYS EVALUATED. The returned state is what
	// this call persists regardless of what happens next.
	rot, nextTaintState := ResolveRuleOfTwo(req.TaintState, req.Taint)
	if stop, err := fold(LayerRuleOfTwo, rot); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: nextTaintState}, err
	}

	// Layer 8: approval policy.
	if stop, err := fold(LayerApprovalPolicy, c.cfg.Approval.Resolve(req.EffectClass)); err != nil || stop {
		return Result{Resolution: accumulated, TaintState: nextTaintState}, err
	}

	// Layer 9: a standing scope can satisfy an outstanding Ask; it can
	// never fire when nothing asked, and it can never touch a Deny (Deny
	// already returned above).
	if accumulated.Decision == Ask {
		for _, scope := range c.cfg.StandingScopes {
			if scope.covers(req.ToolID, req.Namespace) {
				return Result{
					Resolution: Resolution{
						Decision: Allow,
						Layer:    LayerStandingScope,
						Reason:   fmt.Sprintf("standing scope %q satisfied the ask raised at layer %s", scope.Name, accumulated.Layer),
					},
					TaintState: nextTaintState,
				}, nil
			}
		}
		return Result{Resolution: accumulated, TaintState: nextTaintState}, nil
	}

	// Layer 10: otherwise, allow.
	return Result{
		Resolution: Resolution{Decision: Allow, Layer: LayerFallbackAllow, Reason: "no layer objected"},
		TaintState: nextTaintState,
	}, nil
}
