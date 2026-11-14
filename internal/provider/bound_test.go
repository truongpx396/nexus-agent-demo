package provider_test

import (
	"context"
	"testing"

	"github.com/truongpx396/nexus-agent/internal/provider"
)

type stubProvider struct{}

func (stubProvider) Stream(context.Context, provider.Prompt, []provider.ToolSchema, provider.RunContext) (provider.Stream, error) {
	return nil, nil
}

type boundStub struct {
	stubProvider
	model string
}

func (b boundStub) BoundModel() string { return b.model }

func TestServedModel(t *testing.T) {
	if got := provider.ServedModel(stubProvider{}, "routed"); got != "routed" {
		t.Fatalf("a provider with no model of its own serves the routed id, got %q", got)
	}
	if got := provider.ServedModel(boundStub{model: "bound"}, "routed"); got != "bound" {
		t.Fatalf("a bound provider serves its own model, got %q", got)
	}
	if got := provider.ServedModel(boundStub{}, "routed"); got != "routed" {
		t.Fatalf("a provider reporting no model falls back to the routed id, got %q", got)
	}
}

func TestWrap_ReportsABoundModelOnlyWhenEveryProviderServesTheSameOne(t *testing.T) {
	same := provider.Wrap([]provider.Provider{boundStub{model: "m"}, boundStub{model: "m"}})
	if got := provider.ServedModel(same, "routed"); got != "m" {
		t.Fatalf("a chain of one model serves %q, want m", got)
	}
	mixed := provider.Wrap([]provider.Provider{boundStub{model: "a"}, boundStub{model: "b"}})
	if got := provider.ServedModel(mixed, "routed"); got != "routed" {
		t.Fatalf("a chain of mixed models cannot be priced by either, got %q", got)
	}
	unbound := provider.Wrap([]provider.Provider{boundStub{model: "a"}, stubProvider{}})
	if got := provider.ServedModel(unbound, "routed"); got != "routed" {
		t.Fatalf("a chain with an unbound provider cannot be priced, got %q", got)
	}
}
