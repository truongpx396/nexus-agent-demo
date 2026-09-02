package evals

import (
	"context"
	"fmt"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

func TestGateRunOverallPassOnCleanCorpus(t *testing.T) {
	corpus, err := Corpus()
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	cases, err := LoadProviderScriptCases(corpus)
	if err != nil {
		t.Fatalf("LoadProviderScriptCases: %v", err)
	}

	g := &Gate{
		Environment: Environment{Image: "test"},
		// Capability cases are the one class graded by Wilson interval
		// against a threshold rather than exact-match (policy.go): a single
		// trial's interval is too wide to confidently clear 0.9, so this
		// suite (still fully deterministic today — provider/fake, never a
		// live model) runs each capability case enough times for the
		// interval to resolve, exactly the "ship the mechanism before the
		// variance exists to need it" tradeoff KTrials' own doc comment
		// describes.
		KTrials: map[Class]int{ClassCapability: 50},
	}
	result := g.Run(context.Background(), GateInput{
		ProviderCases:   cases,
		PermissionCases: AdversarialPermissionCorpus(),
	})
	if !result.OverallPass {
		t.Fatalf("expected the clean corpus to pass overall: %s (cases: %+v)", result.Detail, result.Cases)
	}
	if result.OverallPassRate != 1.0 {
		t.Fatalf("OverallPassRate = %v, want 1.0", result.OverallPassRate)
	}
}

func TestGateRunBlocksOnASafetyFailureEvenAboveNinetyPercent(t *testing.T) {
	// 19 passing regression-shaped cases plus 1 failing safety case is a
	// 95% overall pass rate — comfortably above the 90% bar — but task
	// 10.1/10.11 both require a safety failure to block regardless.
	var providerCases []ProviderScriptCase
	for i := 0; i < 19; i++ {
		providerCases = append(providerCases, ProviderScriptCase{
			ID:    fmt.Sprintf("regression-%d", i),
			Class: ClassRegression,
			Script: fake.Script{Chunks: []fake.ChunkSpec{
				{Kind: "content", Text: "ok"},
				{Kind: "done", Done: "stop"},
			}},
			Want: WantSpec{FinalText: "ok", Done: "stop"},
		})
	}

	failingSafety := PermissionScenarioCase{
		ID:    "safety-deliberately-broken",
		Class: ClassSafety,
		Run: func() Trial {
			return Trial{CaseID: "safety-deliberately-broken", Verdict: VerdictFail, Detail: "deliberately broken for this test"}
		},
	}

	g := &Gate{Environment: Environment{Image: "test"}}
	result := g.Run(context.Background(), GateInput{
		ProviderCases:   providerCases,
		PermissionCases: []PermissionScenarioCase{failingSafety},
	})

	if result.OverallPass {
		t.Fatalf("a safety-class failure must block even at %.0f%% overall pass rate", result.OverallPassRate*100)
	}
	if result.OverallPassRate < 0.9 {
		t.Fatalf("this test's own premise requires overall pass rate >= 0.9, got %v", result.OverallPassRate)
	}
}

func TestGateRunBlocksOnEfficiencyRegressionEvenWithPerfectQuality(t *testing.T) {
	g := &Gate{Environment: Environment{Image: "test"}}
	result := g.Run(context.Background(), GateInput{
		PermissionCases: AdversarialPermissionCorpus(), // all pass, keeps quality perfect
		EfficiencyCases: []EfficiencyCase{{
			ID:        "efficiency-regressed",
			Class:     ClassCapability,
			Band:      EfficiencyBand{MaxTokens: 100},
			Candidate: EfficiencyMetrics{Tokens: 500},
		}},
	})
	if result.OverallPass {
		t.Fatal("an efficiency band violation must block even when every quality case passed (task 10.8)")
	}
}

func TestGateRunArtifactFiltersToOneArtifact(t *testing.T) {
	g := &Gate{
		Environment: Environment{Image: "test"},
		CaseArtifacts: map[string]ArtifactRef{
			"case-a": {Kind: ArtifactPlan, ID: "triage-plan", Version: 3},
			"case-b": {Kind: ArtifactSkill, ID: "invoice-triage", Version: 1},
		},
	}
	in := GateInput{
		PermissionCases: []PermissionScenarioCase{
			{ID: "case-a", Class: ClassSafety, Run: func() Trial { return Trial{CaseID: "case-a", Verdict: VerdictPass} }},
			{ID: "case-b", Class: ClassSafety, Run: func() Trial { return Trial{CaseID: "case-b", Verdict: VerdictPass} }},
		},
	}

	result := g.RunArtifact(context.Background(), in, ArtifactRef{Kind: ArtifactPlan, ID: "triage-plan", Version: 3})
	if len(result.Cases) != 1 || result.Cases[0].CaseID != "case-a" {
		t.Fatalf("RunArtifact scoped to the plan should run only case-a, got %+v", result.Cases)
	}
}

func TestGateRunEmptyInputNeverPassesVacuously(t *testing.T) {
	g := &Gate{}
	result := g.Run(context.Background(), GateInput{})
	if result.OverallPass {
		t.Fatal("an empty gate run must never resolve OverallPass=true")
	}
}
