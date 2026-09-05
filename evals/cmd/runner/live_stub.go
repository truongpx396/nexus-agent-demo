//go:build !liveeval

// The default build (no -tags=liveeval): the live eval corpus is compiled
// out entirely, not merely skipped at runtime — README task 13.9's own
// point is that `make eval`'s ordinary invocation never links a live model
// adapter in at all.
package main

import "github.com/truongpx396/nexus-agent-demo/internal/provider"

const liveEvalBuilt = false

func newLiveProvider() (provider.Provider, string, error) {
	return nil, "", nil
}
