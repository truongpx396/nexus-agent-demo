// Package evalgate is the one glue point between internal/plan's lifecycle
// gate hook and the hardened release gate (evals.Gate). internal/plan
// deliberately does not import evals — plan.EvalGate's own doc comment
// says so, "so this package doesn't need to depend on evals' own
// corpus/runner shape" — and names cmd/nexusd as where "the real
// evals.Report-backed gate" gets wired in. This package IS that wiring,
// factored out of cmd/nexusd (package main, not importable by a test) so
// it's independently testable and so cmd/nexusd's own role stays "import
// this and pass it to plan.Lifecycle," not "contain the logic."
package evalgate

import (
	"context"

	"github.com/truongpx396/nexus-agent-demo/evals"
	"github.com/truongpx396/nexus-agent-demo/internal/plan"
)

// PlanGate returns a plan.EvalGate closure bound to g and in (README task
// 10.10: "each ... plan ... ships its own versioned cases, run at its
// promotion/enable gate"). Every call scopes g.RunArtifact to exactly the
// (plan_id, version) being gated — a case only counts toward THIS plan
// version's decision if it was tagged for it in g.CaseArtifacts, via
// evals.ArtifactRef{Kind: evals.ArtifactPlan, ID: p.PlanID.String(),
// Version: p.Version}.
//
// A plan version with NO cases tagged for it runs Gate.Run over an empty
// GateInput, which never passes vacuously (evals.Gate's own contract,
// exercised by evals' TestGateRunEmptyInputNeverPassesVacuously) — an
// eval-gated plan promotion with nothing gating it is refused, not
// silently waved through, the same fail-closed default this codebase
// applies everywhere else a decision has no evidence behind it.
func PlanGate(g *evals.Gate, in evals.GateInput) plan.EvalGate {
	return func(ctx context.Context, p plan.Plan) (passed bool, detail string, err error) {
		ref := evals.ArtifactRef{Kind: evals.ArtifactPlan, ID: p.PlanID.String(), Version: p.Version}
		result := g.RunArtifact(ctx, in, ref)
		return result.OverallPass, result.Detail, nil
	}
}
