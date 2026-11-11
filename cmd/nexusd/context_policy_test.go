package main

import (
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/promptctx"
)

func TestContextPolicy_IsOffUntilAskedFor(t *testing.T) {
	t.Setenv("NEXUS_CONTEXT_PRUNING", "")
	t.Setenv("NEXUS_CONDENSE_THRESHOLD_BYTES", "")
	prune, condense := contextPolicy()
	if prune != (promptctx.PrunePolicy{}) || condense != 0 {
		t.Fatalf("default policy = %+v, %d, want both off: enabling either changes what the model sees", prune, condense)
	}
}

func TestContextPolicy_ReadsBothSettings(t *testing.T) {
	t.Setenv("NEXUS_CONTEXT_PRUNING", "true")
	t.Setenv("NEXUS_CONDENSE_THRESHOLD_BYTES", "65536")
	prune, condense := contextPolicy()
	if prune != promptctx.DefaultPrunePolicy() || condense != 65536 {
		t.Fatalf("policy = %+v, %d, want the default prune policy and 65536", prune, condense)
	}

	t.Setenv("NEXUS_CONDENSE_THRESHOLD_BYTES", "-5")
	if _, condense := contextPolicy(); condense != 0 {
		t.Fatalf("a negative threshold = %d, want it read as off", condense)
	}
}
