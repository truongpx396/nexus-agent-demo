package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

type fakeCatalog struct {
	bundles map[string]skills.SkillBundle
	files   map[string]string // "skillID/path" -> content
}

func (c fakeCatalog) Resolve(skillID string) (skills.SkillBundle, bool) {
	b, ok := c.bundles[skillID]
	return b, ok
}

func (c fakeCatalog) ReadFile(skillID, path string) ([]byte, error) {
	v, ok := c.files[skillID+"/"+path]
	if !ok {
		return nil, errNotFound
	}
	return []byte(v), nil
}

var errNotFound = errors.New("not found")

type fakeEvents struct {
	activated         []string
	capabilityIgnored []string
}

func (f *fakeEvents) Activated(_ context.Context, _, _ uuid.UUID, skillID string, held []string) error {
	f.activated = append(f.activated, skillID)
	_ = held
	return nil
}

func (f *fakeEvents) CapabilityIgnored(_ context.Context, _, _ uuid.UUID, skillID, toolID string) error {
	f.capabilityIgnored = append(f.capabilityIgnored, skillID+":"+toolID)
	return nil
}

func newTestRegistry(t *testing.T, refs ...tools.ToolRef) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	for _, ref := range refs {
		tool := stubTool{ref: ref}
		if err := reg.Register(tool); err != nil {
			t.Fatalf("Register(%s): %v", ref, err)
		}
		if err := reg.SetAdmissionStatus(ref, tools.AdmissionClean); err != nil {
			t.Fatalf("SetAdmissionStatus(%s): %v", ref, err)
		}
	}
	return reg
}

// stubTool is a minimal tools.Tool for registry population — its Call/etc.
// are never exercised, only ID/Descriptor via Lookup/AdmissionStatus.
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

func TestActivateSkill_HoldsOnlyIntersectingTools(t *testing.T) {
	held := tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"}
	registry := newTestRegistry(t, held)

	bundle := skills.SkillBundle{
		SkillID:         "triage-report",
		Description:     "Triages a report.",
		DeclaredToolIDs: []string{held.String(), "platform/web_fetch@v1"}, // web_fetch is NOT registered
	}
	events := &fakeEvents{}
	a := ActivateSkill{
		Catalog:  fakeCatalog{bundles: map[string]skills.SkillBundle{"triage-report": bundle}},
		Registry: registry,
		Events:   events,
		Admitted: func(uuid.UUID) []string { return []string{"triage-report"} },
	}

	rc := tools.RunContext{TenantID: uuid.New(), SessionID: uuid.New()}
	in := json.RawMessage(`{"skill_id":"triage-report"}`)
	result, err := a.Call(context.Background(), in, rc)
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Call result is an error: %s", result.Reason)
	}

	var decoded struct {
		HeldToolIDs []string `json:"held_tool_ids"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(decoded.HeldToolIDs) != 1 || decoded.HeldToolIDs[0] != held.String() {
		t.Errorf("held_tool_ids = %v, want [%s] — declared_tool_ids must INTERSECT the resolved catalog, never union", decoded.HeldToolIDs, held)
	}

	if len(events.activated) != 1 || events.activated[0] != "triage-report" {
		t.Errorf("Activated calls = %v, want exactly one for triage-report", events.activated)
	}
	if len(events.capabilityIgnored) != 1 || events.capabilityIgnored[0] != "triage-report:platform/web_fetch@v1" {
		t.Errorf("CapabilityIgnored calls = %v, want exactly one for the absent tool", events.capabilityIgnored)
	}
}

func TestActivateSkill_FailsClosedWhenNotAdmitted(t *testing.T) {
	a := ActivateSkill{
		Catalog:  fakeCatalog{bundles: map[string]skills.SkillBundle{"s": {SkillID: "s"}}},
		Registry: tools.NewRegistry(),
		Events:   &fakeEvents{},
		Admitted: func(uuid.UUID) []string { return nil }, // tenant admits nothing
	}
	result, err := a.Call(context.Background(), json.RawMessage(`{"skill_id":"s"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call succeeded activating a skill the tenant never admitted, want a refusal")
	}
}

func TestActivateSkill_RevocationMidSessionIsReCheckedLive(t *testing.T) {
	// task 7.8: re-checked against what's CURRENTLY admitted on every call,
	// never cached from the first activation in the session.
	admittedNow := []string{"s"}
	a := ActivateSkill{
		Catalog:  fakeCatalog{bundles: map[string]skills.SkillBundle{"s": {SkillID: "s"}}},
		Registry: tools.NewRegistry(),
		Events:   &fakeEvents{},
		Admitted: func(uuid.UUID) []string { return admittedNow },
	}
	in := json.RawMessage(`{"skill_id":"s"}`)

	first, err := a.Call(context.Background(), in, tools.RunContext{})
	if err != nil || first.IsError {
		t.Fatalf("first activation = %+v, %v; want success", first, err)
	}

	admittedNow = nil // tenant config revoked the skill mid-session
	second, err := a.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if !second.IsError {
		t.Fatal("second activation succeeded after revocation, want a refusal — admission must be re-checked live")
	}
}

func TestReadSkillFile_ReturnsReferenceContent(t *testing.T) {
	r := ReadSkillFile{Catalog: fakeCatalog{files: map[string]string{"s/template.md": "# Template"}}}
	result, err := r.Call(context.Background(), json.RawMessage(`{"skill_id":"s","path":"template.md"}`), tools.RunContext{})
	if err != nil || result.IsError {
		t.Fatalf("Call = %+v, %v", result, err)
	}
	var decoded struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Content != "# Template" {
		t.Errorf("Content = %q, want %q", decoded.Content, "# Template")
	}
}

func TestReadSkillFile_RefusesUnknownFile(t *testing.T) {
	r := ReadSkillFile{Catalog: fakeCatalog{}}
	result, err := r.Call(context.Background(), json.RawMessage(`{"skill_id":"s","path":"nope.md"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call succeeded reading a nonexistent file, want a refusal")
	}
}
