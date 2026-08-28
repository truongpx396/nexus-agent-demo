// Command runner is the release gate's CLI entry point (Makefile's `eval`
// target): it loads the corpus, grades every case, prints a table, and
// exits non-zero the moment any case fails or is inconclusive — never on a
// silent inconclusive-as-pass (FR-138).
package main

import (
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/evals"
)

func main() {
	corpus, err := evals.Corpus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval gate: %v\n", err)
		os.Exit(1)
	}
	cases, err := evals.LoadProviderScriptCases(corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval gate: %v\n", err)
		os.Exit(1)
	}

	report := evals.RunProviderScriptCases(cases)

	byID := make(map[string]evals.ProviderScriptCase, len(cases))
	for _, c := range cases {
		byID[c.ID] = c
	}

	fmt.Printf("%-45s %-11s %-8s %s\n", "CASE", "CLASS", "VERDICT", "DETAIL")
	for _, trial := range report.Trials {
		fmt.Printf("%-45s %-11s %-8s %s\n", trial.CaseID, byID[trial.CaseID].Class, trial.Verdict, trial.Detail)
	}

	passed := 0
	for _, trial := range report.Trials {
		if trial.Verdict == evals.VerdictPass {
			passed++
		}
	}
	fmt.Printf("\n%d/%d passed\n", passed, len(report.Trials))

	if !report.Pass() {
		fmt.Println("FAIL")
		os.Exit(1)
	}
	fmt.Println("PASS")
}
