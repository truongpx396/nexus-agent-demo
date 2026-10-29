package runs_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

type fakeAdmitter struct {
	got    controlplane.AdmitRunV1
	result controlplane.AdmitRunResultV1
	err    error
	calls  int
}

func (f *fakeAdmitter) AdmitRun(_ context.Context, req controlplane.AdmitRunV1) (controlplane.AdmitRunResultV1, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

type fakeKeys struct {
	dek    crypto.DEK
	err    error
	calls  int
	onCall func()
}

func (f *fakeKeys) NewDEK(context.Context, uuid.UUID) (crypto.DEK, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	return f.dek, f.err
}

type fakeStarter struct {
	got      runs.Request
	ctxErr   error
	err      error
	calls    int
	delivery <-chan runs.Event
}

func (f *fakeStarter) StartRun(ctx context.Context, req runs.Request) (<-chan runs.Event, error) {
	f.calls++
	f.got, f.ctxErr = req, ctx.Err()
	return f.delivery, f.err
}

type rig struct {
	svc     *runs.Service
	admit   *fakeAdmitter
	keys    *fakeKeys
	starter *fakeStarter
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dek, err := crypto.GenerateDEK("test-key")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	r := &rig{
		admit:   &fakeAdmitter{result: controlplane.AdmitRunResultV1{Admitted: true}},
		keys:    &fakeKeys{dek: dek},
		starter: &fakeStarter{delivery: make(chan runs.Event)},
	}
	r.svc = &runs.Service{Admit: r.admit, Keys: r.keys, Starter: r.starter, CatalogManifestDigest: []byte("catalog-digest")}
	return r
}

func TestLaunch_AdmitsASessionThenStartsTheRunUnderIt(t *testing.T) {
	r := newRig(t)
	tenantID, userID := uuid.New(), uuid.New()
	mcp := []provider.ToolSchema{{Name: "mcp/a@v1"}, {Name: "mcp/b@v1"}}

	got, err := r.svc.Launch(context.Background(), runs.LaunchRequest{
		TenantID: tenantID, UserID: userID, SurfaceID: "telegram", SessionKey: "telegram:42",
		Input: "hello", Conversational: true,
		SkillSetDigest: []byte("skills"), MCPCatalogDigest: []byte("mcp"), MCPCatalog: mcp,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	route := provider.Route(provider.DataLabelInternal, provider.DifficultySimple)
	a := r.admit.got
	if a.TenantID != tenantID || a.UserID != userID || a.SurfaceID != "telegram" || a.SessionKey != "telegram:42" {
		t.Errorf("admission identity = %+v", a)
	}
	if a.DataLabel != "internal" || a.RouteModelID != route.ModelID || a.Autonomy != "supervised" || !a.Conversational {
		t.Errorf("admission defaults/route = label %q model %q autonomy %q conversational %v", a.DataLabel, a.RouteModelID, a.Autonomy, a.Conversational)
	}
	wantDigest := harness.Digest(harness.Config{
		SystemPromptVersion: "phase2-v1", CatalogManifestDigest: []byte("catalog-digest"),
		SkillSetDigest: []byte("skills"), PromptMode: "phase2-single-shot", MCPCatalogDigest: []byte("mcp"),
	})
	if !bytes.Equal(a.HarnessDigest, wantDigest) {
		t.Errorf("harness digest does not fold in the catalog, skill-set and MCP digests")
	}

	s := r.starter.got
	if got.SessionID == uuid.Nil || s.SessionID != got.SessionID || a.SessionID != got.SessionID {
		t.Errorf("session ids disagree: launched %s, admitted %s, started %s", got.SessionID, a.SessionID, s.SessionID)
	}
	if s.TenantID != tenantID || s.Input != "hello" || s.ModelID != route.ModelID || s.AutonomyLevel != "supervised" || !s.Conversational {
		t.Errorf("run request = %+v", s)
	}
	if len(s.ExtraLoadedTools) != 2 || s.ExtraLoadedTools[0] != "mcp/a@v1" || s.ExtraLoadedTools[1] != "mcp/b@v1" || len(s.ExtraCatalog) != 2 {
		t.Errorf("MCP catalog not carried onto the run: catalog %d, loaded %v", len(s.ExtraCatalog), s.ExtraLoadedTools)
	}
	if got.Events == nil {
		t.Error("Launched.Events is nil")
	}

	// The run's Seal must be the session's own key bound to this tenant and
	// session: what it seals opens under the DEK Launch asked the provider for.
	sealed, _, keyID, err := s.Seal([]byte("payload"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	plain, err := crypto.Open(r.keys.dek, sealed, tenantID.String(), got.SessionID.String())
	if err != nil || string(plain) != "payload" || keyID != r.keys.dek.KeyID {
		t.Errorf("Seal does not round-trip under the minted key: %q, %v, key %q", plain, err, keyID)
	}
}

func TestLaunch_PassesExplicitRoutingAndAutonomyThrough(t *testing.T) {
	r := newRig(t)
	_, err := r.svc.Launch(context.Background(), runs.LaunchRequest{
		TenantID: uuid.New(), UserID: uuid.New(), SurfaceID: "rest", Input: "x",
		DataLabel: provider.DataLabelRestricted, Difficulty: provider.DifficultyComplex,
		Autonomy: "read_only", BudgetUSD: "0.05",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	route := provider.Route(provider.DataLabelRestricted, provider.DifficultyComplex)
	a := r.admit.got
	if a.DataLabel != "restricted" || a.RouteModelID != route.ModelID || a.Autonomy != "read_only" || a.BudgetUSD != "0.05" {
		t.Errorf("admission = %+v", a)
	}
	if a.SessionKey != "" {
		t.Errorf("SessionKey = %q, want empty so the control plane addresses the session by its id", a.SessionKey)
	}
}

func TestLaunch_StartsTheRunWithoutTheCallersCancellation(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.keys.onCall = cancel // the request is abandoned after admission, before the run starts

	if _, err := r.svc.Launch(ctx, runs.LaunchRequest{TenantID: uuid.New(), UserID: uuid.New(), SurfaceID: "rest"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if r.starter.ctxErr != nil {
		t.Fatalf("StartRun saw a cancelled context (%v): a run must outlive the request that started it", r.starter.ctxErr)
	}
}

func TestLaunch_FailuresStopTheSequenceAtTheFailingStep(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name       string
		setup      func(*rig)
		wantAs     any
		wantIs     error
		keysCalled bool
		startCalls int
	}{
		{"control plane error", func(r *rig) { r.admit.err = errBoom }, nil, errBoom, false, 0},
		{"control plane refuses", func(r *rig) {
			r.admit.result = controlplane.AdmitRunResultV1{Admitted: false, Reason: "invalid budget_usd"}
		}, new(*runs.NotAdmittedError), nil, false, 0},
		{"key provisioning error", func(r *rig) { r.keys.err = errBoom }, nil, errBoom, true, 0},
		{"starter error", func(r *rig) { r.starter.err = errBoom }, nil, errBoom, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(r)

			_, err := r.svc.Launch(context.Background(), runs.LaunchRequest{TenantID: uuid.New(), UserID: uuid.New(), SurfaceID: "rest"})
			if err == nil {
				t.Fatal("Launch succeeded, want an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if tc.wantAs != nil {
				var na *runs.NotAdmittedError
				if !errors.As(err, &na) || na.Reason != "invalid budget_usd" {
					t.Errorf("error %v is not a *NotAdmittedError carrying the control plane's reason", err)
				}
			}
			if (r.keys.calls > 0) != tc.keysCalled {
				t.Errorf("key provider called %d time(s), want called=%v", r.keys.calls, tc.keysCalled)
			}
			if r.starter.calls != tc.startCalls {
				t.Errorf("starter called %d time(s), want %d", r.starter.calls, tc.startCalls)
			}
		})
	}
}
