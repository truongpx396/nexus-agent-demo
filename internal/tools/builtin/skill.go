package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

var activateSkillRef = tools.ToolRef{Namespace: "platform", Name: "activate_skill", Version: "v1"}
var readSkillFileRef = tools.ToolRef{Namespace: "platform", Name: "read_skill_file", Version: "v1"}

// SkillResolver is the lookup ActivateSkill needs — satisfied structurally
// by *skills.Catalog; declared here so this tool depends on exactly the one
// method it calls, the same granularity SandboxExec/BlobStore/Claims already
// use elsewhere in this package's siblings.
type SkillResolver interface {
	Resolve(skillID string) (skills.SkillBundle, bool)
}

// SkillFileReader is the lookup ReadSkillFile needs.
type SkillFileReader interface {
	ReadFile(skillID, path string) ([]byte, error)
}

// SkillEvents records the audit-only events skill activation produces
// (README task 7.8: skill_activated/skill_capability_ignored) — declared
// here exactly like tools.Claims is declared in internal/tools/claims.go, so
// neither kernel nor internal/tools/pipeline.go need to learn skills exist:
// activate_skill is dispatched through the ordinary 16-step pipeline like
// any other tool (task 7.7 — "no new ABI"). internal/runctl.
// SkillEventRecorder implements this for real, mirroring ClaimTracker;
// cmd/nexusd wires it in.
type SkillEvents interface {
	Activated(ctx context.Context, tenantID, sessionID uuid.UUID, skillID string, heldToolIDs []string) error
	CapabilityIgnored(ctx context.Context, tenantID, sessionID uuid.UUID, skillID, toolID string) error
}

// ActivateSkill implements activate_skill(skill_id): an ordinary Tool, no
// new kernel ABI. Its tool_result IS the skill's tier-2 body — extending the
// byte-stable prefix the same way every tool result already does (this
// file's own package doc, and kernel/loop.go's existing tool_result append,
// need zero changes for this to work).
type ActivateSkill struct {
	Catalog  SkillResolver
	Registry *tools.Registry
	Events   SkillEvents
	// Admitted returns tenantID's currently admitted skill ids — read fresh
	// on every call (task 7.8: re-checked against what's CURRENTLY
	// admitted, never cached from session start; tenant config can move
	// between admission and activation).
	Admitted func(tenantID uuid.UUID) []string
}

type activateSkillInput struct {
	SkillID string `json:"skill_id"`
}

func (ActivateSkill) ID() tools.ToolRef { return activateSkillRef }

func (ActivateSkill) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          activateSkillRef,
		Description: "Activates a resident skill by id, returning its body and which of its declared tools are actually held.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"skill_id":{"type":"string"}},"required":["skill_id"]}`),
		EffectClass: tools.EffectClassReadOnly,
	}
}

// Taint: a skill's body is untrusted content until proven otherwise — same
// posture as any tool result, even one that already passed bundle
// admission (admission screens the bundle, not any one particular reader's
// trust of it).
func (ActivateSkill) Taint() tools.Taint {
	return tools.Taint{ReturnsUntrusted: true, ReadsPrivateData: false, MutatesExternal: false}
}

func (ActivateSkill) IsConcurrencySafe(json.RawMessage) bool { return true } // a read has nothing to race against

func (ActivateSkill) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (ActivateSkill) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req activateSkillInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.SkillID == "" {
		return fmt.Errorf("skill_id is required")
	}
	return nil
}

func (a ActivateSkill) Call(ctx context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req activateSkillInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}

	var admitted []string
	if a.Admitted != nil {
		admitted = a.Admitted(rc.TenantID)
	}
	if !containsString(admitted, req.SkillID) {
		return tools.Result{IsError: true, Reason: "skill_not_admitted: " + req.SkillID + " is not in this tenant's admitted skill set"}, nil
	}

	bundle, ok := a.Catalog.Resolve(req.SkillID)
	if !ok {
		return tools.Result{IsError: true, Reason: "unknown_skill: " + req.SkillID}, nil
	}

	// Re-check every declared_tool_id against the CURRENTLY resolved
	// catalog (task 7.8), never against a stale snapshot from session
	// start — declared_tool_ids INTERSECTS what's actually held, never
	// unions (task 7.4): an absent entry is ignored, recorded, and the run
	// continues on what it holds, fail closed rather than fatal.
	var held []string
	for _, declID := range bundle.DeclaredToolIDs {
		if a.isHeld(declID) {
			held = append(held, declID)
		} else if a.Events != nil {
			if err := a.Events.CapabilityIgnored(ctx, rc.TenantID, rc.SessionID, req.SkillID, declID); err != nil {
				return tools.Result{}, fmt.Errorf("record skill_capability_ignored: %w", err)
			}
		}
	}

	if a.Events != nil {
		if err := a.Events.Activated(ctx, rc.TenantID, rc.SessionID, req.SkillID, held); err != nil {
			return tools.Result{}, fmt.Errorf("record skill_activated: %w", err)
		}
	}

	out, err := json.Marshal(map[string]any{
		"skill_id":        bundle.SkillID,
		"description":     bundle.Description,
		"held_tool_ids":   held,
		"reference_files": fileNames(bundle.Files),
	})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}

func (a ActivateSkill) isHeld(declID string) bool {
	if a.Registry == nil {
		return false
	}
	ref, err := tools.ParseToolRef(declID)
	if err != nil {
		return false
	}
	if _, exists := a.Registry.Lookup(ref); !exists {
		return false
	}
	status, _ := a.Registry.AdmissionStatus(ref)
	return status == tools.AdmissionClean
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func fileNames(files []skills.BundleFile) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	return names
}

// ReadSkillFile implements read_skill_file(skill_id, path): lazy tier 3
// (task 7.9), reference/template content only — a bundle's script is never
// fetched this way (internal/skills.Catalog.ReadFile itself refuses the
// reserved "script" name; this tool adds no additional check beyond
// delegating to it).
type ReadSkillFile struct {
	Catalog SkillFileReader
}

type readSkillFileInput struct {
	SkillID string `json:"skill_id"`
	Path    string `json:"path"`
}

func (ReadSkillFile) ID() tools.ToolRef { return readSkillFileRef }

func (ReadSkillFile) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          readSkillFileRef,
		Description: "Reads one reference file from a skill's bundle by path.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"skill_id":{"type":"string"},"path":{"type":"string"}},"required":["skill_id","path"]}`),
		EffectClass: tools.EffectClassReadOnly,
	}
}

func (ReadSkillFile) Taint() tools.Taint {
	return tools.Taint{ReturnsUntrusted: true, ReadsPrivateData: false, MutatesExternal: false}
}

func (ReadSkillFile) IsConcurrencySafe(json.RawMessage) bool { return true }

func (ReadSkillFile) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (ReadSkillFile) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req readSkillFileInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.SkillID == "" || req.Path == "" {
		return fmt.Errorf("skill_id and path are required")
	}
	return nil
}

func (r ReadSkillFile) Call(_ context.Context, in json.RawMessage, _ tools.RunContext) (tools.Result, error) {
	var req readSkillFileInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	content, err := r.Catalog.ReadFile(req.SkillID, req.Path)
	if err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	out, err := json.Marshal(map[string]string{"content": string(content)})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}
