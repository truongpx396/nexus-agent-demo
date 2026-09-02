package kernel

import (
	"context"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// PipelineExecutor adapts *tools.Pipeline (the real ToolExecutor, README
// task 3.4) to this package's ToolExecutor seam. The translation lives
// here, in kernel, rather than in internal/tools, because this package's
// own doc comment (kernel/types.go) allows kernel to import tools but never
// the reverse — internal/tools has no reason to know kernel.ToolResult
// exists, and giving it one would be the cycle
// tests/contract/boundaries_test.go's import-graph rules exist to catch.
type PipelineExecutor struct {
	Pipeline *tools.Pipeline
}

// ResetTurn forwards to tools.Pipeline.ResetTurn — kernel/loop.go calls this
// once per turn through the optional `interface{ ResetTurn() }` check
// rather than as part of ToolExecutor itself, so an executor with nothing
// to reset (kernel.NotImplementedToolExecutor) needs no no-op method.
func (p PipelineExecutor) ResetTurn() { p.Pipeline.ResetTurn() }

func (p PipelineExecutor) Execute(ctx context.Context, req ToolUseRequest, rc ExecContext) ToolResult {
	out := p.Pipeline.Execute(ctx, tools.Invocation{
		TenantID:      rc.TenantID,
		SessionID:     rc.SessionID,
		ToolName:      req.ToolName,
		Input:         req.Input,
		AutonomyLevel: rc.AutonomyLevel,
	})
	return ToolResult{
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
	}
}

// ExecuteApproved implements kernel.ApprovedExecutor (README task 5.7) by
// forwarding to tools.Pipeline.ExecuteApproved — the resume-time digest
// re-verification path Kernel.Resume calls instead of Execute.
func (p PipelineExecutor) ExecuteApproved(ctx context.Context, req ToolUseRequest, approvedDigest []byte, rc ExecContext) ToolResult {
	out := p.Pipeline.ExecuteApproved(ctx, tools.Invocation{
		TenantID:      rc.TenantID,
		SessionID:     rc.SessionID,
		ToolName:      req.ToolName,
		Input:         req.Input,
		AutonomyLevel: rc.AutonomyLevel,
	}, approvedDigest)
	return ToolResult{
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
	}
}
