package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
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

// registryWith and stubTool reuse skill_test.go's own newTestRegistry/
// stubTool (same package) under a name local to this file's own tests —
// registryWith just forwards, so each test reads self-contained.
func registryWith(t *testing.T, refs ...tools.ToolRef) *tools.Registry {
	return newTestRegistry(t, refs...)
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
	d := Delegate{Ledger: fakeLedger{depth: maxDelegationDepth}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (a session at the depth bound must not delegate further)", res.Decision)
	}
}

func TestDelegate_CheckPermissions_DeniesAtConcurrentBound(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{open: maxConcurrentDelegations, total: 1}, Registry: reg}
	res := d.CheckPermissions(context.Background(), delegateInputJSON(t, "platform/file_read@v1"), tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (concurrent bound reached)", res.Decision)
	}
}

func TestDelegate_CheckPermissions_DeniesAtPerRunBound(t *testing.T) {
	reg := registryWith(t, tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"})
	d := Delegate{Ledger: fakeLedger{total: maxDelegationsPerRun}, Registry: reg}
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
	got     SpawnRequest
}

func (f *fakeSpawner) Spawn(_ context.Context, req SpawnRequest) (uuid.UUID, error) {
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
