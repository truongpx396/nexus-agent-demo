// Package kerneladapter is the driven adapter that lets the agent loop call the
// tool pipeline: it implements kernel.ToolExecutor, kernel.ApprovedExecutor and
// kernel.TaintSeeder over *tools.Pipeline.
//
// It lives here, not in kernel/, so the core of the hexagon names its ports
// without importing the pipeline behind them: kernel/ imports no internal/tools
// (tests/contract/boundaries_test.go), and neither does the pipeline import
// kernel/ — only this adapter knows both.
package kerneladapter

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// PipelineExecutor adapts *tools.Pipeline (the real ToolExecutor, README
// task 3.4) to kernel's ToolExecutor seam. The translation lives in this
// adapter package rather than in kernel (which must not import tools) or in
// internal/tools (which has no reason to know kernel.ToolResult exists).
type PipelineExecutor struct {
	Pipeline *tools.Pipeline
}

// ResetTurn forwards to tools.Pipeline.ResetTurn — kernel/loop.go calls this
// once per turn through the optional `interface{ ResetTurn() }` check
// rather than as part of ToolExecutor itself, so an executor with nothing
// to reset (kernel.NotImplementedToolExecutor) needs no no-op method.
func (p PipelineExecutor) ResetTurn() { p.Pipeline.ResetTurn() }

func (p PipelineExecutor) Execute(ctx context.Context, req kernel.ToolUseRequest, rc kernel.ExecContext) kernel.ToolResult {
	out := p.Pipeline.Execute(ctx, tools.Invocation{
		TenantID:      rc.TenantID,
		SessionID:     rc.SessionID,
		ToolName:      req.ToolName,
		Input:         req.Input,
		AutonomyLevel: rc.AutonomyLevel,
	})
	return kernel.ToolResult{
		Output:             out.Output,
		IsError:            out.IsError,
		Reason:             out.Reason,
		PermissionDenied:   out.PermissionDenied,
		AwaitingApproval:   out.AwaitingApproval,
		AskKind:            out.AskKind,
		CanonicalDigest:    out.CanonicalDigest,
		ApprovalMismatch:   out.ApprovalMismatch,
		EffectClass:        out.EffectClass,
		AwaitingDelegation: out.AwaitingDelegation,
		ChildSessionID:     out.ChildSessionID,
		TaintChanged:       out.TaintChanged,
		TaintEngaged:       out.TaintEngaged,
	}
}

// SeedTaint implements kernel.TaintSeeder (kernel/types.go's own doc
// comment) by forwarding to tools.Pipeline.SeedTaint.
func (p PipelineExecutor) SeedTaint(sessionID uuid.UUID, autonomyLevel string, engaged [3]bool) {
	p.Pipeline.SeedTaint(sessionID, autonomyLevel, engaged)
}

// ExecuteApproved implements kernel.ApprovedExecutor (README task 5.7) by
// forwarding to tools.Pipeline.ExecuteApproved — the resume-time digest
// re-verification path Kernel.Resume calls instead of Execute.
func (p PipelineExecutor) ExecuteApproved(ctx context.Context, req kernel.ToolUseRequest, approvedDigest []byte, rc kernel.ExecContext) kernel.ToolResult {
	out := p.Pipeline.ExecuteApproved(ctx, tools.Invocation{
		TenantID:      rc.TenantID,
		SessionID:     rc.SessionID,
		ToolName:      req.ToolName,
		Input:         req.Input,
		AutonomyLevel: rc.AutonomyLevel,
	}, approvedDigest)
	return kernel.ToolResult{
		Output:             out.Output,
		IsError:            out.IsError,
		Reason:             out.Reason,
		PermissionDenied:   out.PermissionDenied,
		AwaitingApproval:   out.AwaitingApproval,
		AskKind:            out.AskKind,
		CanonicalDigest:    out.CanonicalDigest,
		ApprovalMismatch:   out.ApprovalMismatch,
		EffectClass:        out.EffectClass,
		AwaitingDelegation: out.AwaitingDelegation,
		ChildSessionID:     out.ChildSessionID,
		TaintChanged:       out.TaintChanged,
		TaintEngaged:       out.TaintEngaged,
	}
}
