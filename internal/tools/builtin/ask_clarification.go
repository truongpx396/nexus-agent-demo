package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

var askClarificationRef = tools.ToolRef{Namespace: "platform", Name: "ask_clarification", Version: "v1"}

type askClarificationInput struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// AskClarification implements ask_clarification(question, options?): a tool
// with no real effect at all, whose only job is to FORCE a human-in-the-loop
// pause -- CheckPermissions always returns "ask", regardless of autonomy
// level (internal/permissions.Chain.Resolve evaluates Gate 2 unconditionally
// at layer 5, before the autonomy-driven layers can resolve it any other
// way, and nothing can downgrade an already-Ask'd decision back to Allow
// except a standing scope that names this exact tool -- none will exist for
// a brand new tool id). That reuses the SAME suspend/resume path every other
// Ask-outcome tool call already goes through (kernel.Kernel.Resume,
// oversight.Approvals, REST's existing GET/POST /v1/approvals*) rather than
// building a second one: the web UI recognizes a pending approval as a
// clarification purely by tool_id, and always resolves it via
// GrantModified(session, {"answer": ...}) -- never a bare grant, since a
// bare grant would re-execute with the ORIGINAL {question, options} input
// and Call has no question of its own to answer. A denial is the human
// declining to answer at all, exactly like denying any other ask.
type AskClarification struct{}

func (AskClarification) ID() tools.ToolRef { return askClarificationRef }

func (AskClarification) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          askClarificationRef,
		Description: "Pauses and asks the human a clarifying question before continuing. Use when the task is genuinely ambiguous and guessing would risk doing the wrong thing. options, if given, are shown as choices; otherwise the human answers freely.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}},"required":["question"]}`),
		EffectClass: tools.EffectClassReadOnly,
	}
}

// Taint: a human's direct answer to the model's own question is neither
// untrusted retrieved content nor private data pulled from some other
// source -- the same trust position an approval grant already has, not
// platform/retrieve's or platform/web_fetch's.
func (AskClarification) Taint() tools.Taint {
	return tools.Taint{ReturnsUntrusted: false, ReadsPrivateData: false, MutatesExternal: false}
}

func (AskClarification) IsConcurrencySafe(json.RawMessage) bool { return true } // nothing local to race on

// CheckPermissions always asks: this tool's only purpose is a human pause,
// so it never defers to the autonomy-driven layers the way an ordinary
// read-only tool does.
func (AskClarification) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "ask", Reason: "ask_clarification always requires a human answer"}
}

func (AskClarification) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req askClarificationInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.Question == "" {
		return fmt.Errorf("question is required")
	}
	return nil
}

// Call only ever runs at resume time (ExecuteApproved, after a human grants
// with modified input carrying their answer) -- CheckPermissions' Ask never
// lets a fresh Execute reach here. It has nothing of its own to compute: it
// just echoes back whatever input the resume carried, which by the web UI's
// own contract (this type's doc comment) is {"answer": ...}, so the model's
// next turn sees exactly that as the tool_result.
func (AskClarification) Call(_ context.Context, in json.RawMessage, _ tools.RunContext) (tools.Result, error) {
	return tools.Result{Output: in}, nil
}
