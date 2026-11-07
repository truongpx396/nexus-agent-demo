package rest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	oversightrest "github.com/truongpx396/nexus-agent-demo/internal/oversight/rest"
	runsrest "github.com/truongpx396/nexus-agent-demo/internal/runs/rest"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// These are the REST surface's conformance tests (README task 7.12:
// "conformance test per surface"). They live beside the Descriptor but are an
// external package because they assemble the surface from the feature modules
// that make it up, which the kit itself must not import.

// TestDescriptor_StreamingRouteIsWired checks Descriptor's SupportsStreaming
// claim against the actual mux, not just a struct literal. A 404 here would
// mean the capability declaration and the real route registration have drifted
// apart.
func TestDescriptor_StreamingRouteIsWired(t *testing.T) {
	if !restkit.Descriptor.SupportsStreaming {
		t.Fatal("Descriptor.SupportsStreaming = false; this test assumes REST claims streaming support")
	}
	s := restkit.NewServer(nil, runsrest.NewHandler(nil, nil))
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/00000000-0000-0000-0000-000000000000/events", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /v1/runs/{id}/events = 404, want the streaming route Descriptor.SupportsStreaming claims to actually be registered")
	}
}

// TestDescriptor_ApprovalRoutesAreCapabilityGated checks that the approval
// endpoints exist exactly when the oversight module is mounted, which is how a
// deployment that has not wired oversight shows as not offering approvals.
func TestDescriptor_ApprovalRoutesAreCapabilityGated(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals", nil)

	without := restkit.NewServer(nil, runsrest.NewHandler(nil, nil))
	rec := httptest.NewRecorder()
	without.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/approvals with the oversight module unmounted = %d, want 404", rec.Code)
	}

	with := restkit.NewServer(nil, runsrest.NewHandler(nil, nil), oversightrest.Handler{})
	rec2 := httptest.NewRecorder()
	with.Handler().ServeHTTP(rec2, req)
	if rec2.Code == http.StatusNotFound {
		t.Error("GET /v1/approvals with the oversight module mounted = 404, want the route registered")
	}
}
