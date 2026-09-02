// Command runner is the release gate's CLI entry point (Makefile's `eval`
// target): it loads every suite (README task 10.1's ~20-case corpus across
// regression/capability/safety/negative), grades it through evals.Gate
// (k trials, Wilson intervals, class policies, held-out gap, efficiency
// gating — tasks 10.2-10.8), checks it against a committed baseline for
// interval-separation regressions (task 10.2/10.11), prints a per-case
// table, and exits non-zero the moment the gate does not clear "≥90% pass
// AND zero regressions" (task 10.11) — never on a silent inconclusive-as-
// pass (FR-138).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/truongpx396/nexus-agent-demo/evals"
)

// capabilityKTrials is how many times a capability-class case runs before
// its Wilson interval is asked to clear the 0.9 default threshold — see
// evals.Gate's own KTrials doc comment: every grader in this corpus is
// still deterministic today (provider/fake, the permission chain, the
// admission scanners), so this is "enough repeated deterministic
// observations for the interval to resolve," not "genuine measured
// variance." 50 clears the threshold with room to spare (evals/policy_test.go
// works the exact math).
const capabilityKTrials = 50

func defaultEnvironment() evals.Environment {
	return evals.Environment{
		Image:        "provider-fake+in-process", // no sandbox/model actually used by this gate's suites today
		ResourceBand: "ci-standard",
		Concurrency:  1,
		Region:       envOr("NEXUS_REGION", "local"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func buildInput() (evals.GateInput, error) {
	corpus, err := evals.Corpus()
	if err != nil {
		return evals.GateInput{}, err
	}
	providerCases, err := evals.LoadProviderScriptCases(corpus)
	if err != nil {
		return evals.GateInput{}, err
	}

	heldOutFS, err := evals.HeldOutCorpus()
	if err != nil {
		return evals.GateInput{}, err
	}
	heldOutCases, err := evals.LoadProviderScriptCases(heldOutFS)
	if err != nil {
		return evals.GateInput{}, err
	}

	descriptorCases, skillCases := evals.AdversarialAdmissionCorpus()

	return evals.GateInput{
		ProviderCases:        providerCases,
		HeldOutProviderCases: heldOutCases,
		PermissionCases:      evals.AdversarialPermissionCorpus(),
		DescriptorCases:      descriptorCases,
		SkillCases:           skillCases,
		TrajectoryCases:      evals.TrajectoryCorpus(),
		EfficiencyCases:      evals.EfficiencyCorpus(),
		// JudgeCases intentionally omitted from the default CI run: no
		// live/pinned model is wired into `make eval` (constitution
		// Principle IX — correctness tests never call a live model), so a
		// Gate with no Judge configured resolves every JudgeCase
		// Inconclusive rather than silently skipping it — see the JUDGE row
		// in the printed table.
		JudgeCases: evals.JudgeCorpus(),
	}, nil
}

func main() {
	updateBaseline := flag.Bool("update-baseline", false, "write the current run's stats as the new committed baseline instead of checking against it")
	baselinePath := flag.String("baseline", defaultBaselinePath(), "path to the committed baseline JSON file")
	flag.Parse()

	in, err := buildInput()
	if err != nil {
		fatalf("eval gate: %v", err)
	}

	gate := &evals.Gate{
		Environment:    defaultEnvironment(),
		KTrials:        map[evals.Class]int{evals.ClassCapability: capabilityKTrials},
		MinOverallPass: 0.9, // task 10.11
	}
	result := gate.Run(context.Background(), in)

	if *updateBaseline {
		if err := evals.SaveBaseline(*baselinePath, result.ToBaseline()); err != nil {
			fatalf("eval gate: %v", err)
		}
		fmt.Printf("baseline written to %s (%d cases)\n", *baselinePath, len(result.Cases))
		return
	}

	if baseline, err := evals.LoadBaseline(*baselinePath); err == nil {
		regressions, rerr := evals.CheckRegressions(baseline, result)
		if rerr != nil {
			fatalf("eval gate: %v", rerr)
		}
		result.ApplyRegressions(regressions)
	} else {
		fmt.Fprintf(os.Stderr, "eval gate: no baseline at %s yet (%v) — regression check skipped; run with -update-baseline once the corpus is stable\n", *baselinePath, err)
	}

	printReport(result)

	if !result.OverallPass {
		fmt.Println("FAIL")
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func printReport(result evals.GateResult) {
	fmt.Printf("environment: %s (digest %s)\n\n", envSummary(result.Environment), result.Environment.DigestHex()[:12])
	fmt.Printf("%-52s %-11s %-13s %-11s %s\n", "CASE", "CLASS", "VERDICT", "INTERVAL", "DETAIL")
	for _, c := range result.Cases {
		fmt.Printf("%-52s %-11s %-13s [%.2f,%.2f]  %s\n", c.CaseID, c.Class, c.Verdict, c.Interval.Low, c.Interval.High, c.Detail)
	}

	passed := 0
	for _, c := range result.Cases {
		if c.Verdict == evals.VerdictPass {
			passed++
		}
	}
	fmt.Printf("\n%d/%d passed (%.1f%%), %d regression(s), held-out gap=%.3f (within=%v)\n",
		passed, len(result.Cases), result.OverallPassRate*100, len(result.Regressions), result.HeldOutGap, result.HeldOutWithin)
	if len(result.Regressions) > 0 {
		fmt.Printf("regressions: %v\n", result.Regressions)
	}
	fmt.Println(result.Detail)
}

func envSummary(e evals.Environment) string {
	return fmt.Sprintf("image=%s resource_band=%s concurrency=%d region=%s", e.Image, e.ResourceBand, e.Concurrency, e.Region)
}

// defaultBaselinePath is relative to the process's CURRENT WORKING
// DIRECTORY, not this source file — Go binaries carry no notion of "where
// I was built from" at runtime. That's fine in practice: the Makefile's
// `eval` target (and CI's eval-gate job) always run `go run
// ./evals/cmd/runner` from the repo root, so this resolves to the one
// committed file either way; a caller running it from elsewhere can pass
// -baseline explicitly.
func defaultBaselinePath() string {
	return filepath.Join("evals", "testdata", "baseline.json")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
