package evals

import (
	"encoding/json"
	"fmt"
	"os"
)

// CaseStat is one case's recorded outcome from a gate run — the shape a
// Baseline persists per case id, and the shape a fresh GateResult is
// reduced to before comparing against one.
type CaseStat struct {
	Class    Class
	Pass, N  int
	Interval Interval
	Verdict  Verdict
}

// Baseline is "last known good": an Environment (task 10.3's pin) plus a
// per-case CaseStat, committed as a file next to the corpus it was measured
// against (evals/testdata/baseline.json) the same way a golden-file test
// commits its expected output. gate.go's Run doesn't load or compare
// against this on its own — CheckRegressions is the explicit, separate step
// that does, so a caller that only wants "does the corpus pass" never pays
// for a baseline file that doesn't exist yet (e.g. the very first run that
// creates one).
type Baseline struct {
	Environment Environment
	Cases       map[string]CaseStat
}

// ToBaseline reduces a GateResult to the Baseline shape SaveBaseline
// persists — the thing a maintainer commits once a candidate's own gate run
// is the new "last known good."
func (r GateResult) ToBaseline() Baseline {
	b := Baseline{Environment: r.Environment, Cases: make(map[string]CaseStat, len(r.Cases))}
	for _, c := range r.Cases {
		b.Cases[c.CaseID] = CaseStat{Class: c.Class, Pass: c.Pass, N: c.N, Interval: c.Interval, Verdict: c.Verdict}
	}
	return b
}

// LoadBaseline reads a committed baseline file. A missing file is reported
// as an error, never as an empty Baseline — the caller (the runner's
// --update-baseline flag, or CheckRegressions) decides what a missing
// baseline means for ITS purpose; this function never guesses "so there's
// nothing to regress against."
func LoadBaseline(path string) (Baseline, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is an operator/CLI-flag-controlled config value (evals/cmd/runner's -baseline flag), never end-user input
	if err != nil {
		return Baseline{}, fmt.Errorf("evals: load baseline %s: %w", path, err)
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return Baseline{}, fmt.Errorf("evals: parse baseline %s: %w", path, err)
	}
	return b, nil
}

// SaveBaseline writes b as pretty-printed JSON — a diffable, reviewable
// file, the same reasoning any committed golden file in this codebase
// follows.
func SaveBaseline(path string, b Baseline) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("evals: marshal baseline: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("evals: write baseline %s: %w", path, err)
	}
	return nil
}

// CheckRegressions is task 10.2's regression definition, applied against a
// stored baseline, and task 10.3's refusal made literal: comparing across a
// different eval_environment_digest is refused outright (ErrEnvironmentMismatch),
// never silently attempted. A case present in candidate but absent from
// baseline (a newly added case) is never a regression — there is nothing to
// have regressed FROM.
func CheckRegressions(baseline Baseline, candidate GateResult) ([]string, error) {
	if err := CompareEnvironments(baseline.Environment, candidate.Environment); err != nil {
		return nil, err
	}
	var regressions []string
	for _, c := range candidate.Cases {
		base, ok := baseline.Cases[c.CaseID]
		if !ok {
			continue
		}
		if c.Interval.Separated(base.Interval) {
			regressions = append(regressions, c.CaseID)
		}
	}
	return regressions, nil
}

// ApplyRegressions folds a CheckRegressions result back into r: task 10.11's
// "zero regressions" becomes part of OverallPass, and Regressions/Detail
// both reflect it. Gate.Run never calls this itself (it has no baseline to
// compare against — see Baseline's own doc comment); the runner calls it
// once it has loaded one.
func (r *GateResult) ApplyRegressions(regressions []string) {
	r.Regressions = regressions
	if len(regressions) == 0 {
		return
	}
	r.OverallPass = false
	r.Detail = fmt.Sprintf("BLOCKED: %d case(s) regressed against baseline (interval separation): %v", len(regressions), regressions)
}
