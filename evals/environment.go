package evals

import (
	"crypto/sha256"
	"fmt"
)

// Environment pins everything about WHERE a trial ran that could change its
// outcome independent of the case itself (README task 10.3) — the eval-gate
// counterpart to internal/harness.Config, which pins WHAT ran. Comparing a
// candidate report against a baseline recorded under a different Environment
// would blame a config/behavior change for what was actually an
// infrastructure change (or vice versa hide one behind the other), so
// gate.go refuses the comparison outright rather than silently proceeding.
type Environment struct {
	// Image identifies the sandbox/build image trials ran in — the eval
	// counterpart of internal/sandbox's own image pin.
	Image string
	// ResourceBand is a coarse resource class ("standard", "large", ...),
	// not exact CPU/mem figures — those vary run to run even on identical
	// hardware; the band is what's actually behavior-relevant.
	ResourceBand string
	// Concurrency is how many trials this environment ran in parallel —
	// concurrency itself can change timing-sensitive outcomes (a stuck
	// detector's window, a hook timeout), so it is part of the pinned
	// identity, not an incidental runner setting.
	Concurrency int
	// Region names the deployment region trials ran against — data
	// residency and per-region routing (README §3, pattern 66's `region`
	// seam) can change which model/route a case actually exercises.
	Region string
}

// Digest returns a stable digest over e, the same fixed-field-order /
// crypto/sha256 construction internal/harness.Digest uses, for the same
// reason: deterministic across process restarts, sensitive to every field.
func (e Environment) Digest() []byte {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "image=%s\n", e.Image)
	_, _ = fmt.Fprintf(h, "resource_band=%s\n", e.ResourceBand)
	_, _ = fmt.Fprintf(h, "concurrency=%d\n", e.Concurrency)
	_, _ = fmt.Fprintf(h, "region=%s\n", e.Region)
	return h.Sum(nil)
}

// DigestHex is Digest formatted for a report table / dashboard column.
func (e Environment) DigestHex() string { return fmt.Sprintf("%x", e.Digest()) }

// ErrEnvironmentMismatch is returned by CompareEnvironments (and by
// gate.go's baseline regression check, which calls it before trusting a
// stored baseline) when the two runs being compared did not happen under
// the same eval_environment_digest.
type ErrEnvironmentMismatch struct {
	Baseline, Candidate Environment
}

func (e ErrEnvironmentMismatch) Error() string {
	return fmt.Sprintf(
		"evals: refusing to compare across eval_environment_digest: baseline=%s candidate=%s",
		e.Baseline.DigestHex(), e.Candidate.DigestHex(),
	)
}

// CompareEnvironments is the task 10.3 refusal: comparing trial results
// recorded under different environments is never silently allowed.
func CompareEnvironments(baseline, candidate Environment) error {
	if baseline.DigestHex() != candidate.DigestHex() {
		return ErrEnvironmentMismatch{Baseline: baseline, Candidate: candidate}
	}
	return nil
}
