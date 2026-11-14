package main

import "github.com/truongpx396/nexus-agent/internal/promptctx"

// contextPolicy is how the kernel keeps a long conversation inside the model's
// context. Both controls exist in the kernel and used to be unreachable from the
// binary: Kernel.PrunePolicy and Kernel.CondenseThresholdBytes were never set, so
// every turn sent the whole transcript until the provider reported an overflow and
// the run ended context_overflow.
//
// Both are off by default. They change what the model is shown, so enabling one
// is a prompt change that has to clear the eval gate against the model in use
// (`make eval`, and the live track), which this process cannot judge.
//
//   - NEXUS_CONTEXT_PRUNING=true trims old tool results in the per-turn view:
//     soft-trimmed past 4 KiB, cleared past 16 KiB, the last 20 messages kept
//     whole (promptctx.DefaultPrunePolicy). The logged events are never touched.
//   - NEXUS_CONDENSE_THRESHOLD_BYTES=N replaces the older part of the transcript
//     with a summary once it passes N bytes. With no condenser model configured the
//     summary is the model-free extractive pass, so it costs no model call.
func contextPolicy() (prune promptctx.PrunePolicy, condenseAtBytes int) {
	if envOr("NEXUS_CONTEXT_PRUNING", "false") == "true" {
		prune = promptctx.DefaultPrunePolicy()
	}
	return prune, max(envIntOr("NEXUS_CONDENSE_THRESHOLD_BYTES", 0), 0)
}
