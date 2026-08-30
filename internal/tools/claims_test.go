package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeClaims is an in-memory Claims implementation for pipeline tests —
// mirrors the shape internal/runctl's real, store-backed implementation
// will have (keyed by (sessionID, digest string)), without any DB
// dependency, the same reason fakeTool exists instead of exercising a
// builtin tool.
type fakeClaims struct {
	mu     sync.Mutex
	byKey  map[string]uuid.UUID // (sessionID,digest) -> claimID
	status map[uuid.UUID]string // claimID -> "in_flight" | "completed" | "abandoned"
	opens  int
	closes int
}

func newFakeClaims() *fakeClaims {
	return &fakeClaims{byKey: map[string]uuid.UUID{}, status: map[uuid.UUID]string{}}
}

func claimKey(sessionID uuid.UUID, digest []byte) string {
	return sessionID.String() + ":" + string(digest)
}

func (f *fakeClaims) Open(_ context.Context, _, sessionID uuid.UUID, _ string, digest []byte) (uuid.UUID, ClaimOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	key := claimKey(sessionID, digest)
	if id, ok := f.byKey[key]; ok {
		switch f.status[id] {
		case "completed":
			return id, ClaimDone, nil
		default: // in_flight or abandoned-but-still-mapped is treated as ambiguous by this fake, matching the real Open's own "never re-execute" default
			return id, ClaimAmbiguous, nil
		}
	}
	id := uuid.New()
	f.byKey[key] = id
	f.status[id] = "in_flight"
	return id, ClaimFresh, nil
}

func (f *fakeClaims) Complete(_ context.Context, _, _, claimID uuid.UUID, failed bool, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	if failed {
		f.status[claimID] = "abandoned"
		return nil
	}
	f.status[claimID] = "completed"
	return nil
}

func TestPipeline_Claims_OpenedBeforeCallAndCompletedAfter(t *testing.T) {
	tool := newFakeTool("platform", "send_email", EffectClassExternal)
	h := newHarness(t, tool)
	claims := newFakeClaims()
	h.cfg.Claims = claims
	p := h.pipeline()

	inv := h.invocation("autonomous", `{"to":"finance@example.com"}`)
	got := p.Execute(context.Background(), inv)
	if got.IsError {
		t.Fatalf("Execute() = %+v, want a clean success", got)
	}
	if tool.callCount != 1 {
		t.Fatalf("tool called %d times, want 1", tool.callCount)
	}
	if claims.opens != 1 || claims.closes != 1 {
		t.Fatalf("claims.opens=%d claims.closes=%d, want exactly one open and one close", claims.opens, claims.closes)
	}
}

func TestPipeline_Claims_ReadOnlyToolNeverOpensAClaim(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	claims := newFakeClaims()
	h.cfg.Claims = claims
	p := h.pipeline()

	p.Execute(context.Background(), h.invocation("read_only", `{"path":"a.txt"}`))
	if claims.opens != 0 {
		t.Fatalf("a read-only tool must never open a claim (task 6.6 scopes claims to non-read-only effects); opens=%d", claims.opens)
	}
}

// TestPipeline_Claims_InFlightAmbiguityRefusesASecondCall is task 6.6's own
// core guarantee made concrete: a claim left in_flight (simulating a
// process that crashed between Open and Complete) must refuse a second
// identical call outright rather than re-executing it.
func TestPipeline_Claims_InFlightAmbiguityRefusesASecondCall(t *testing.T) {
	tool := newFakeTool("platform", "send_email", EffectClassExternal)
	h := newHarness(t, tool)
	claims := newFakeClaims()
	h.cfg.Claims = claims
	p := h.pipeline()

	sessionID, tenantID := uuid.New(), uuid.New()
	inv := Invocation{TenantID: tenantID, SessionID: sessionID, ToolName: h.tool.ref.String(), Input: json.RawMessage(`{"to":"finance@example.com"}`), AutonomyLevel: "autonomous"}

	// Pre-seed an in_flight claim for this EXACT digest, simulating a crash
	// between a prior Open and its Complete.
	digest, err := CanonicalDigest(h.tool.ref.String(), inv.Input)
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	stuckID := uuid.New()
	claims.byKey[claimKey(sessionID, digest)] = stuckID
	claims.status[stuckID] = "in_flight"

	got := p.Execute(context.Background(), inv)
	if !got.IsError {
		t.Fatalf("Execute() = %+v, want a refusal for an ambiguous in-flight claim", got)
	}
	if tool.callCount != 0 {
		t.Fatalf("tool.callCount = %d, want 0 — an ambiguous claim must never be re-executed", tool.callCount)
	}
}

func TestPipeline_Claims_CompletedClaimShortCircuits(t *testing.T) {
	tool := newFakeTool("platform", "send_email", EffectClassExternal)
	h := newHarness(t, tool)
	claims := newFakeClaims()
	h.cfg.Claims = claims
	p := h.pipeline()

	sessionID, tenantID := uuid.New(), uuid.New()
	inv := Invocation{TenantID: tenantID, SessionID: sessionID, ToolName: h.tool.ref.String(), Input: json.RawMessage(`{"to":"finance@example.com"}`), AutonomyLevel: "autonomous"}

	first := p.Execute(context.Background(), inv)
	if first.IsError {
		t.Fatalf("first Execute() = %+v, want success", first)
	}
	if tool.callCount != 1 {
		t.Fatalf("after the first call, tool.callCount = %d, want 1", tool.callCount)
	}

	// A second, identical call must short-circuit — the claim already
	// completed.
	second := p.Execute(context.Background(), inv)
	if !second.IsError {
		t.Fatalf("second Execute() = %+v, want a short-circuit refusal", second)
	}
	if tool.callCount != 1 {
		t.Fatalf("after the second (short-circuited) call, tool.callCount = %d, want still 1 — never re-executed", tool.callCount)
	}
}
