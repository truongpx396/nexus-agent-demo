// Package exechook is the command-hook adapter: it runs an operator-configured
// local script per invocation (README task 3.11's "command" hook kind). It owns
// the os/exec dependency so the hook port and dispatcher in internal/hooks —
// and the tool pipeline that imports them — do not.
package exechook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
)

// Handler runs an operator-configured local script per invocation.
// cfg.Command is config the operator authored (README task 3.11's "command"
// hook kind), never attacker-controlled input — the whole feature is
// "run this script", so exec.CommandContext with a variable argv is the
// point, not a vulnerability.
type Handler struct{}

func (Handler) Run(ctx context.Context, cfg hooks.Config, hctx hooks.Context) (hooks.Outcome, error) {
	payload, err := json.Marshal(hooks.Request{Event: string(cfg.Event), ToolID: hctx.ToolID, Input: hctx.Input})
	if err != nil {
		return hooks.Outcome{}, fmt.Errorf("marshal command hook request: %w", err)
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...) //nolint:gosec // operator-authored hook config (hooks.Config.Command), not attacker input
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return hooks.Outcome{}, fmt.Errorf("command hook %q: %w (stderr: %s)", cfg.Name, err, stderr.String())
	}

	var resp hooks.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return hooks.Outcome{}, fmt.Errorf("command hook %q: parse response: %w", cfg.Name, err)
	}
	return hooks.Outcome{Decision: hooks.Decision(resp.Decision), Reason: resp.Reason, UpdatedInput: resp.UpdatedInput}, nil
}
