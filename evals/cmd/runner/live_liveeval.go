//go:build liveeval

// README task 13.9 (closing production-readiness finding F6): this file is
// the ONLY place a live model is ever constructed for the eval gate, and it
// only compiles in under `-tags=liveeval` — `make eval` (no tag) never
// builds it, so the scripted provider/fake suite stays the CI-gating
// harness suite exactly as it is today.
package main

import (
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/anthropic"
)

const liveEvalBuilt = true

// newLiveProvider constructs the real Anthropic adapter for LiveTaskCorpus.
// A missing ANTHROPIC_API_KEY is reported, not silently skipped — running
// `go run -tags=liveeval ./evals/cmd/runner` without a key is a
// configuration mistake worth a clear error, distinct from simply not
// passing the tag at all (main.go's own liveEvalBuilt branch handles that
// case separately).
func newLiveProvider() (provider.Provider, string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, "", fmt.Errorf("ANTHROPIC_API_KEY is required to run the live eval corpus (-tags=liveeval)")
	}
	model := envOr("NEXUS_LIVEEVAL_MODEL", "claude-sonnet-5")
	return anthropic.New(apiKey, model), model, nil
}
