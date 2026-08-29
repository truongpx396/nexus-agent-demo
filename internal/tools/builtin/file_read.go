package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// FileRead reads one file from the session workspace.
type FileRead struct{}

var fileReadRef = tools.ToolRef{Namespace: "platform", Name: "file_read", Version: "v1"}

type fileReadInput struct {
	Path string `json:"path"`
}

func (FileRead) ID() tools.ToolRef { return fileReadRef }

func (FileRead) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          fileReadRef,
		Description: "Reads a text file from the session workspace and returns its contents.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		EffectClass: tools.EffectClassReadOnly,
	}
}

// Taint: file content is untrusted (it could contain injected instructions
// or be attacker-planted); reading the local workspace touches no private
// data source and mutates nothing.
func (FileRead) Taint() tools.Taint {
	return tools.Taint{ReturnsUntrusted: true, ReadsPrivateData: false, MutatesExternal: false}
}

func (FileRead) IsConcurrencySafe(json.RawMessage) bool { return true } // a read has nothing to race against

func (FileRead) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (FileRead) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req fileReadInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.Path == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

func (FileRead) Call(_ context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req fileReadInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	full, err := resolveWorkspacePath(rc.WorkspaceDir, req.Path)
	if err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	content, err := os.ReadFile(full) //nolint:gosec // full is resolved through resolveWorkspacePath's traversal guard, never a raw join of caller input
	if err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	out, err := json.Marshal(map[string]string{"content": string(content)})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}
