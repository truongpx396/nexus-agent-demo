package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// fakeOversightPort is a minimal OversightPort for route-registration tests
// — its methods are never meant to be exercised, only its non-nilness.
type fakeOversightPort struct{}

func (fakeOversightPort) GetApproval(context.Context, uuid.UUID, uuid.UUID) (ApprovalView, error) {
	return ApprovalView{}, nil
}
func (fakeOversightPort) ListPendingApprovals(context.Context, uuid.UUID) ([]ApprovalView, error) {
	return nil, nil
}
func (fakeOversightPort) Grant(context.Context, uuid.UUID, uuid.UUID, string) ResumeOutcome {
	return ResumeOutcome{}
}
func (fakeOversightPort) GrantModified(context.Context, uuid.UUID, uuid.UUID, string, json.RawMessage) ResumeOutcome {
	return ResumeOutcome{}
}
func (fakeOversightPort) Deny(context.Context, uuid.UUID, uuid.UUID, string, string) ResumeOutcome {
	return ResumeOutcome{}
}

// TestDescriptor_StreamingRouteIsWired is this surface's conformance test
// (README task 7.12: "conformance test per surface") — checks Descriptor's
// SupportsStreaming claim against the actual mux, not just a struct
// literal. A 404 here would mean the capability declaration and the real
// route registration have drifted apart.
func TestDescriptor_StreamingRouteIsWired(t *testing.T) {
	if !Descriptor.SupportsStreaming {
		t.Fatal("Descriptor.SupportsStreaming = false; this test assumes REST claims streaming support")
	}
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/00000000-0000-0000-0000-000000000000/events", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /v1/runs/{id}/events = 404, want the streaming route Descriptor.SupportsStreaming claims to actually be registered")
	}
}

func TestDescriptor_ApprovalRoutesAreCapabilityGated(t *testing.T) {
	withoutOversight := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals", nil)
	rec := httptest.NewRecorder()
	withoutOversight.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/approvals with Oversight unset = %d, want 404 (unmounted)", rec.Code)
	}

	withOversight := &Server{Oversight: fakeOversightPort{}}
	rec2 := httptest.NewRecorder()
	withOversight.Handler().ServeHTTP(rec2, req)
	if rec2.Code == http.StatusNotFound {
		t.Error("GET /v1/approvals with Oversight set = 404, want the route mounted")
	}
}
