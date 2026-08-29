package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// FileWrite writes one file into the session workspace, creating parent
// directories as needed.
type FileWrite struct{}

var fileWriteRef = tools.ToolRef{Namespace: "platform", Name: "file_write", Version: "v1"}

type fileWriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (FileWrite) ID() tools.ToolRef { return fileWriteRef }

func (FileWrite) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          fileWriteRef,
		Description: "Writes a text file into the session workspace, creating parent directories as needed. Overwrites an existing file at the same path.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		EffectClass: tools.EffectClassMutating,
	}
}

// Taint: a write changes state (the constitution's Rule of Two counts
// "change state or communicate externally" as one combined leg —
// MutatesExternal here). Output is this tool's own confirmation, not
// untrusted content; it reads no private data source.
func (FileWrite) Taint() tools.Taint {
	return tools.Taint{ReturnsUntrusted: false, ReadsPrivateData: false, MutatesExternal: true}
}

func (FileWrite) IsConcurrencySafe(json.RawMessage) bool { return false } // concurrent writes to the same workspace can interleave

func (FileWrite) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (FileWrite) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req fileWriteInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.Path == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

func (FileWrite) Call(_ context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req fileWriteInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	full, err := resolveWorkspacePath(rc.WorkspaceDir, req.Path)
	if err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	if err := os.WriteFile(full, []byte(req.Content), 0o600); err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	out, err := json.Marshal(map[string]any{"path": req.Path, "bytes_written": len(req.Content)})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}
