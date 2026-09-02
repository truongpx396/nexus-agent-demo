package evals

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
)

// DescriptorAdmissionCase grades internal/tools.Scan directly (task 3.3):
// the "descriptor swap after admission" adversarial shape named in the
// testing-strategy table (README §7) — a descriptor that scanned clean once
// is swapped for one carrying an injection payload, and re-scanning the
// SWAPPED descriptor must catch it. Scan is pure and in-process, so this is
// a code grader exactly like provider_case.go's, just aimed at a different
// piece of production logic (task 10.4's "code graders wherever the
// criterion is objectively checkable").
type DescriptorAdmissionCase struct {
	ID          string
	Class       Class
	Description string
	Original    tools.Descriptor
	Swapped     tools.Descriptor
	WantSwapped tools.AdmissionStatus
}

func RunDescriptorAdmissionCases(cases []DescriptorAdmissionCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, gradeDescriptorAdmission(c))
	}
	return report
}

func gradeDescriptorAdmission(c DescriptorAdmissionCase) Trial {
	origStatus, _ := tools.Scan(c.Original)
	if origStatus != tools.AdmissionClean {
		return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("fixture error: original descriptor scanned %s, not clean — the swap wouldn't be a swap", origStatus)}
	}
	swappedStatus, findings := tools.Scan(c.Swapped)
	if swappedStatus != c.WantSwapped {
		return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("re-scan after swap = %s, want %s (findings: %v)", swappedStatus, c.WantSwapped, findings)}
	}
	return Trial{CaseID: c.ID, Verdict: VerdictPass, Detail: fmt.Sprintf("swap correctly re-caught as %s (%v)", swappedStatus, findings)}
}

// SkillCapabilityWideningCase grades internal/tools/builtin.ActivateSkill.
// Call directly — the real production code path task 7.4/7.8 describe, not
// a paraphrase of it: a bundle that DECLARES a tool id the tenant's catalog
// never admitted (or never scanned clean) must have that id dropped and
// recorded as skill_capability_ignored, never folded into `held_tool_ids`.
// Registry/RunContext are both in-process value types (no DB), so this runs
// exactly as deterministically as the permission-chain suite.
type SkillCapabilityWideningCase struct {
	ID          string
	Class       Class
	Description string
	Bundle      skills.SkillBundle
	// HeldRefs is what the tenant's catalog actually admits and has scanned
	// clean — deliberately a STRICT SUBSET of Bundle.DeclaredToolIDs, so the
	// case is a genuine widening attempt, not a no-op.
	HeldRefs []tools.ToolRef
}

func RunSkillCapabilityWideningCases(cases []SkillCapabilityWideningCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, gradeSkillCapabilityWidening(c))
	}
	return report
}

func gradeSkillCapabilityWidening(c SkillCapabilityWideningCase) Trial {
	registry := tools.NewRegistry()
	namespaces := map[string]bool{}
	for _, ref := range c.HeldRefs {
		if !namespaces[ref.Namespace] {
			if err := registry.DeclareNamespace(ref.Namespace, "evals-fixture"); err != nil {
				return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("fixture error: DeclareNamespace: %v", err)}
			}
			namespaces[ref.Namespace] = true
		}
		if err := registry.Register(admissionStubTool{ref: ref}); err != nil {
			return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("fixture error: Register(%s): %v", ref, err)}
		}
		if err := registry.SetAdmissionStatus(ref, tools.AdmissionClean); err != nil {
			return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("fixture error: SetAdmissionStatus(%s): %v", ref, err)}
		}
	}

	events := &capabilityIgnoredRecorder{}
	activate := builtin.ActivateSkill{
		Catalog:  singleBundleCatalog{bundle: c.Bundle},
		Registry: registry,
		Events:   events,
		Admitted: func(uuid.UUID) []string { return []string{c.Bundle.SkillID} },
	}

	in, err := json.Marshal(map[string]string{"skill_id": c.Bundle.SkillID})
	if err != nil {
		return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("fixture error: marshal input: %v", err)}
	}
	result, err := activate.Call(context.Background(), in, tools.RunContext{TenantID: uuid.New(), SessionID: uuid.New()})
	if err != nil {
		return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("ActivateSkill.Call error: %v", err)}
	}
	if result.IsError {
		return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("ActivateSkill.Call refused: %s", result.Reason)}
	}

	var out struct {
		HeldToolIDs []string `json:"held_tool_ids"`
	}
	if err := json.Unmarshal(result.Output, &out); err != nil {
		return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("fixture error: unmarshal result: %v", err)}
	}

	held := make(map[string]bool, len(c.HeldRefs))
	for _, ref := range c.HeldRefs {
		held[ref.String()] = true
	}
	for _, gotID := range out.HeldToolIDs {
		if !held[gotID] {
			return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("held_tool_ids contains %q, which the tenant catalog never admitted — union, not intersection", gotID)}
		}
	}

	var notHeld []string
	for _, declID := range c.Bundle.DeclaredToolIDs {
		if !held[declID] {
			notHeld = append(notHeld, declID)
		}
	}
	for _, wantIgnored := range notHeld {
		wantKey := c.Bundle.SkillID + ":" + wantIgnored
		if !events.contains(wantKey) {
			return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("declared_tool_id %q was dropped but never recorded as skill_capability_ignored", wantIgnored)}
		}
	}

	return Trial{CaseID: c.ID, Verdict: VerdictPass, Detail: fmt.Sprintf("held=%v, capability_ignored=%v — declared_tool_ids intersected, never unioned", out.HeldToolIDs, events.capabilityIgnored)}
}

type singleBundleCatalog struct{ bundle skills.SkillBundle }

func (c singleBundleCatalog) Resolve(skillID string) (skills.SkillBundle, bool) {
	if skillID != c.bundle.SkillID {
		return skills.SkillBundle{}, false
	}
	return c.bundle, true
}

type capabilityIgnoredRecorder struct {
	activated         []string
	capabilityIgnored []string
}

func (r *capabilityIgnoredRecorder) Activated(_ context.Context, _, _ uuid.UUID, skillID string, held []string) error {
	r.activated = append(r.activated, skillID)
	_ = held
	return nil
}

func (r *capabilityIgnoredRecorder) CapabilityIgnored(_ context.Context, _, _ uuid.UUID, skillID, toolID string) error {
	r.capabilityIgnored = append(r.capabilityIgnored, skillID+":"+toolID)
	return nil
}

func (r *capabilityIgnoredRecorder) contains(key string) bool {
	for _, v := range r.capabilityIgnored {
		if v == key {
			return true
		}
	}
	return false
}

// admissionStubTool is a minimal tools.Tool for populating a Registry in
// these fixtures — only ID/Descriptor are ever read (via Lookup/
// AdmissionStatus), the same narrow stub internal/tools/builtin/
// skill_test.go's own stubTool is, reimplemented here rather than imported
// (that one lives in a _test.go file in a different package and is not
// importable).
type admissionStubTool struct{ ref tools.ToolRef }

func (s admissionStubTool) ID() tools.ToolRef { return s.ref }
func (s admissionStubTool) Descriptor() tools.Descriptor {
	return tools.Descriptor{ID: s.ref, Description: "evals fixture stub", EffectClass: tools.EffectClassReadOnly}
}
func (admissionStubTool) Taint() tools.Taint                     { return tools.DefaultTaint() }
func (admissionStubTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (admissionStubTool) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}
func (admissionStubTool) ValidateInput(context.Context, json.RawMessage, tools.RunContext) error {
	return nil
}
func (admissionStubTool) Call(context.Context, json.RawMessage, tools.RunContext) (tools.Result, error) {
	return tools.Result{}, fmt.Errorf("evals fixture stub: Call is never exercised by these cases")
}
