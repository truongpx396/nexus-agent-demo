// Package builtin ships the five tools README task 3.12 names —
// file_read, file_write, file_search, shell, web_fetch — each declaring a
// Taint and an EffectClass. None of them enforce their own safety: the
// permission chain (internal/permissions) and hooks (internal/hooks) are
// what bound what these tools may do, exactly as the Phase 3 demo's "delete
// the build dir" refusal happens at the chain, not inside the shell tool
// itself. There is no sandbox yet (that's Phase 5's internal/sandbox) —
// shell and file_write operate directly on a per-session workspace
// directory, not inside any container or namespace.
package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveWorkspacePath maps a tool-supplied relative path onto an absolute
// path under workspaceDir, refusing anything that would escape it. Cleaning
// "/" + relPath first collapses any ../ segments against a virtual root
// BEFORE joining onto workspaceDir, so "../../etc/passwd" becomes
// "/etc/passwd" and then safely joins to "<workspace>/etc/passwd" — it
// cannot walk upward past workspaceDir no matter how many ../ segments it
// contains.
func resolveWorkspacePath(workspaceDir, relPath string) (string, error) {
	if workspaceDir == "" {
		return "", fmt.Errorf("no workspace directory configured for this session")
	}
	if relPath == "" {
		return "", fmt.Errorf("path is required")
	}
	cleaned := filepath.Clean("/" + relPath)
	full := filepath.Join(workspaceDir, cleaned)

	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace directory: %w", err)
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if absFull != absWorkspace && !strings.HasPrefix(absFull, absWorkspace+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the session workspace", relPath)
	}
	return absFull, nil
}
