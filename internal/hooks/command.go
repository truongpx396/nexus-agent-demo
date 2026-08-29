package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// commandRequest/commandResponse are the JSON hooks/command scripts and
// hooks/http endpoints share over stdin/POST-body and stdout/response-body
// respectively — one wire shape for both handlers.
type commandRequest struct {
	Event  string          `json:"event"`
	ToolID string          `json:"tool_id"`
	Input  json.RawMessage `json:"input"`
}

type commandResponse struct {
	Decision     string          `json:"decision"`
	Reason       string          `json:"reason,omitempty"`
	UpdatedInput json.RawMessage `json:"updated_tool_input,omitempty"`
}

// CommandHandler runs an operator-configured local script per invocation.
// cfg.Command is config the operator authored (README task 3.11's "command"
// hook kind), never attacker-controlled input — the whole feature is
// "run this script", so exec.CommandContext with a variable argv is the
// point, not a vulnerability.
type CommandHandler struct{}

func (CommandHandler) Run(ctx context.Context, cfg Config, hctx Context) (Outcome, error) {
	payload, err := json.Marshal(commandRequest{Event: string(cfg.Event), ToolID: hctx.ToolID, Input: hctx.Input})
	if err != nil {
		return Outcome{}, fmt.Errorf("marshal command hook request: %w", err)
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...) //nolint:gosec // operator-authored hook config (Config.Command), not attacker input
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Outcome{}, fmt.Errorf("command hook %q: %w (stderr: %s)", cfg.Name, err, stderr.String())
	}

	var resp commandResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return Outcome{}, fmt.Errorf("command hook %q: parse response: %w", cfg.Name, err)
	}
	return Outcome{Decision: Decision(resp.Decision), Reason: resp.Reason, UpdatedInput: resp.UpdatedInput}, nil
}
