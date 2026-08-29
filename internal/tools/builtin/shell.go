package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// Shell runs a command in the session workspace. It is deliberately
// UNSANDBOXED — Phase 5's internal/sandbox is what will run this inside
// Docker with --network none and hard resource limits. Until then, what
// this tool may do is bounded entirely by the permission chain
// (internal/permissions) and hooks (internal/hooks) upstream of it — the
// Phase 3 demo's "delete the build dir" refusal happens there, never here.
type Shell struct {
	// Timeout bounds wall-clock execution; defaults to 30s.
	Timeout time.Duration
}

var shellRef = tools.ToolRef{Namespace: "platform", Name: "shell", Version: "v1"}

type shellInput struct {
	Cmd string `json:"cmd"`
}

func (Shell) ID() tools.ToolRef { return shellRef }

func (Shell) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          shellRef,
		Description: "Runs a shell command in the session workspace and returns its combined stdout/stderr.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`),
		EffectClass: tools.EffectClassMutating,
	}
}

// Taint: every field true — arbitrary output, may read anything the process
// can reach, may change or communicate anything the process can reach. This
// is the fail-closed default (tools.DefaultTaint) made explicit rather than
// relied upon, since "shell" is exactly the tool a narrower declaration
// would be dishonest for.
func (Shell) Taint() tools.Taint { return tools.DefaultTaint() }

func (Shell) IsConcurrencySafe(json.RawMessage) bool { return false } // an arbitrary command may touch shared workspace state

func (Shell) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (Shell) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req shellInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.Cmd == "" {
		return fmt.Errorf("cmd is required")
	}
	return nil
}

func (s Shell) Call(ctx context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req shellInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", req.Cmd) //nolint:gosec // running an arbitrary command IS this tool's job; internal/permissions bounds it, not this call site
	if rc.WorkspaceDir != "" {
		cmd.Dir = rc.WorkspaceDir
	}
	// WaitDelay bounds how long Wait (inside CombinedOutput) will wait, once
	// the context is done, for the killed process's own I/O to actually
	// finish — without it, a command that backgrounds or forks a
	// grandchild sharing the same stdout/stderr pipe (e.g. "sleep 5 &", or
	// a shell that doesn't tail-call-optimize "sleep 5" into an exec) can
	// keep that pipe open long after the direct child was killed, and
	// CombinedOutput blocks until the grandchild exits on its own — the
	// exact hang github.com/golang/go/issues/23019 added WaitDelay to fix.
	cmd.WaitDelay = 500 * time.Millisecond
	output, runErr := cmd.CombinedOutput()

	out, err := json.Marshal(map[string]string{"output": string(output)})
	if err != nil {
		return tools.Result{}, err
	}
	if runErr != nil {
		return tools.Result{Output: out, IsError: true, Reason: runErr.Error()}, nil
	}
	return tools.Result{Output: out}, nil
}
