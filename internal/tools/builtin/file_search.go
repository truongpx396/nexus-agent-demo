package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// FileSearch finds files in the session workspace whose path matches a
// shell-style glob (filepath.Match semantics — no "**"; that keeps the
// pattern language a closed, well-understood one rather than growing a
// bespoke globbing dialect for a Phase 3 demo tool).
type FileSearch struct{}

var fileSearchRef = tools.ToolRef{Namespace: "platform", Name: "file_search", Version: "v1"}

const fileSearchMaxResults = 500

type fileSearchInput struct {
	Pattern string `json:"pattern"`
}

func (FileSearch) ID() tools.ToolRef { return fileSearchRef }

func (FileSearch) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          fileSearchRef,
		Description: `Searches the session workspace for files whose relative path matches a glob pattern (e.g. "*.go", "src/*.md").`,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`),
		EffectClass: tools.EffectClassReadOnly,
	}
}

// Taint: the list of matched paths is derived from workspace content an
// earlier untrusted step may have created, so it is treated as untrusted
// output; it reads no private data source and mutates nothing.
func (FileSearch) Taint() tools.Taint {
	return tools.Taint{ReturnsUntrusted: true, ReadsPrivateData: false, MutatesExternal: false}
}

func (FileSearch) IsConcurrencySafe(json.RawMessage) bool { return true }

func (FileSearch) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}

func (FileSearch) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req fileSearchInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.Pattern == "" {
		return fmt.Errorf("pattern is required")
	}
	if _, err := filepath.Match(req.Pattern, "probe"); err != nil {
		return fmt.Errorf("invalid glob pattern: %w", err)
	}
	return nil
}

func (FileSearch) Call(_ context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req fileSearchInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	if rc.WorkspaceDir == "" {
		return tools.Result{IsError: true, Reason: "no workspace directory configured for this session"}, nil
	}

	var matches []string
	walkErr := filepath.WalkDir(rc.WorkspaceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(rc.WorkspaceDir, path)
		if relErr != nil {
			return relErr
		}
		matched, matchErr := filepath.Match(req.Pattern, rel)
		if matchErr != nil {
			return matchErr
		}
		if !matched {
			matched, matchErr = filepath.Match(req.Pattern, filepath.Base(rel))
			if matchErr != nil {
				return matchErr
			}
		}
		if matched {
			matches = append(matches, rel)
			if len(matches) >= fileSearchMaxResults {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return tools.Result{IsError: true, Reason: walkErr.Error()}, nil
	}

	out, err := json.Marshal(map[string]any{"matches": matches, "truncated": len(matches) >= fileSearchMaxResults})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}
