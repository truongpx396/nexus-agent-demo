package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/delegate"
	"github.com/truongpx396/nexus-agent/internal/tools"
)

type fakeLedger struct {
	depth       int
	root        uuid.UUID
	open, total int
}

func (f fakeLedger) ParentContext(context.Context, uuid.UUID, uuid.UUID) (int, uuid.UUID, error) {
	return f.depth, f.root, nil
}

func (f fakeLedger) CountForRoot(context.Context, uuid.UUID, uuid.UUID) (int, int, error) {
	return f.open, f.total, nil
}

// registryWith returns a registry with refs registered and admitted, for the
// scope-grant checks that look a tool up by reference.
func registryWith(t *testing.T, refs ...tools.ToolRef) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	for _, ref := range refs {
		if err := reg.Register(stubTool{ref: ref}); err != nil {
			t.Fatalf("Register(%s): %v", ref, err)
		}
		if err := reg.SetAdmissionStatus(ref, tools.AdmissionClean); err != nil {
			t.Fatalf("SetAdmissionStatus(%s): %v", ref, err)
		}
	}
	return reg
}

// stubTool is a minimal tools.Tool for registry population: its Call and the
// rest are never exercised, only ID and Descriptor through Lookup.
type stubTool struct{ ref tools.ToolRef }

func (s stubTool) ID() tools.ToolRef { return s.ref }
func (s stubTool) Descriptor() tools.Descriptor {
	return tools.Descriptor{ID: s.ref, Description: "stub", EffectClass: tools.EffectClassReadOnly}
}
func (stubTool) Taint() tools.Taint                     { return tools.DefaultTaint() }
func (stubTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (stubTool) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}
func (stubTool) ValidateInput(context.Context, json.RawMessage, tools.RunContext) error { return nil }
func (stubTool) Call(context.Context, json.RawMessage, tools.RunContext) (tools.Result, error) {
	return tools.Result{}, nil
}

func delegateInputJSON(t *testing.T, scopeGrant ...string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(delegateInput{AgentID: "worker", Task: "do the thing", ScopeGrant: scopeGrant})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return b
}

func TestDelegate_CheckPermissions_DeniesAtDepthBound(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{depth: delegate.MaxDepth}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (a session at the depth bound must not delegate further)", res.Decision)
	}
}

func TestDelegate_CheckPermissions_DeniesAtConcurrentBound(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{open: delegate.MaxConcurrent, total: 1}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (concurrent bound reached)", res.Decision)
	}
}

func TestDelegate_CheckPermissions_DeniesAtPerRunBound(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{total: delegate.MaxPerRun}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (per-run bound reached)", res.Decision)
	}
}

func TestDelegate_CheckPermissions_DeniesScopeOutsideAdmittedCatalog(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/shell@v1"), tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (scope_grant names a tool outside the admitted catalog)", res.Decision)
	}
}

func TestDelegate_CheckPermissions_NeverResolvesAllow(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if res.Decision == "allow" {
		t.Fatalf("Gate 2 must never resolve allow (README.md §4) — a within-bounds request must Defer to the chain, not short-circuit it")
	}
	if res.Decision != "defer" {
		t.Fatalf("decision = %q, want defer for a within-bounds, in-catalog request", res.Decision)
	}
}

func TestDelegate_Taint_DefaultsAllThreeLegsTrue(t *testing.T) {
	taint := Delegate{}.Taint()
	if !taint.ReturnsUntrusted || !taint.ReadsPrivateData || !taint.MutatesExternal {
		t.Fatalf("Taint() = %+v, want every leg true (README task 8.9)", taint)
	}
}

type fakeSpawner struct {
	childID uuid.UUID
	err     error
	got     delegate.SpawnRequest
}

func (f *fakeSpawner) Spawn(_ context.Context, req delegate.SpawnRequest) (uuid.UUID, error) {
	f.got = req
	return f.childID, f.err
}

func TestDelegate_Call_ReturnsAwaitingChildSessionID(t *testing.T) {
	childID := uuid.New()
	spawner := &fakeSpawner{childID: childID}
	d := Delegate{Spawner: spawner}
	out, err := d.Call(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.AwaitingChildSessionID == nil || *out.AwaitingChildSessionID != childID {
		t.Fatalf("AwaitingChildSessionID = %v, want %s", out.AwaitingChildSessionID, childID)
	}
	if out.IsError {
		t.Fatalf("Call reported IsError for a successful spawn")
	}
}
