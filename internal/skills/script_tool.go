package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// ScriptTool wraps one bundle's script as an ordinary tools.Tool (README
// task 7.5: "a bundled script registers as a real tool through the ordinary
// gates or the bundle is refused") — dispatched, permission-chained, and
// audited exactly like any builtin tool, never given a privileged execution
// path of its own. Runs through rc.Sandbox when set, exactly like
// builtin.Shell; otherwise a local, unsandboxed os/exec, for tests and any
// caller that hasn't wired one. Registered under the "skill" namespace
// (never "platform") so a bundle's tool identity is visibly distinct from a
// first-party builtin.
type ScriptTool struct {
	SkillID     string
	Description string
	Content     []byte
	Timeout     time.Duration
}

func (s ScriptTool) ID() tools.ToolRef {
	return tools.ToolRef{Namespace: "skill", Name: s.SkillID, Version: "v1"}
}

func (s ScriptTool) Descriptor() tools.Descriptor {
	desc := s.Description
	if desc == "" {
		desc = fmt.Sprintf("Runs the %s skill's bundled script.", s.SkillID)
	}
	return tools.Descriptor{
		ID:          s.ID(),
		Description: desc,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		EffectClass: tools.EffectClassMutating,
	}
}

// Taint: every field true, the same fail-closed default builtin.Shell makes
// explicit for the same reason — a skill's script is exactly as unbounded
// as any other arbitrary-command tool.
func (ScriptTool) Taint() tools.Taint { return tools.DefaultTaint() }

func (ScriptTool) IsConcurrencySafe(json.RawMessage) bool { return false }

func (ScriptTool) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (ScriptTool) ValidateInput(context.Context, json.RawMessage, tools.RunContext) error { return nil }

func (s ScriptTool) Call(ctx context.Context, _ json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	if rc.Sandbox != nil {
		output, exitCode, breach, err := rc.Sandbox.Exec(ctx, string(s.Content))
		if err != nil {
			return tools.Result{IsError: true, Reason: err.Error()}, nil
		}
		out, merr := json.Marshal(map[string]string{"output": output})
		if merr != nil {
			return tools.Result{}, merr
		}
		if breach != "" {
			return tools.Result{Output: out, IsError: true, Reason: "sandbox_breach: " + breach}, nil
		}
		if exitCode != 0 {
			return tools.Result{Output: out, IsError: true, Reason: fmt.Sprintf("exit status %d", exitCode)}, nil
		}
		return tools.Result{Output: out}, nil
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", string(s.Content)) //nolint:gosec // s.Content is admitted skill-bundle content, run exactly like builtin.Shell runs an arbitrary command — internal/permissions bounds it, not this call site
	if rc.WorkspaceDir != "" {
		cmd.Dir = rc.WorkspaceDir
	}
	cmd.WaitDelay = 500 * time.Millisecond // see builtin.Shell's identical field for why
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
