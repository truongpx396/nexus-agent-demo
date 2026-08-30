// Package memory is file-first per-tenant memory (README task 7.1, pattern
// #46): content lives as plain files on disk, one directory per tenant,
// injected immutably at session start — a write here takes effect the NEXT
// session, never the one that made it, which is what keeps the byte-stable
// prefix (internal/promptctx) honest. Nothing here is durable-runtime-state
// in the append-only-log sense (Principle II); the files are config-like,
// reconstructible data, the same way a skill bundle on disk is.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Store roots every tenant's memory files under RootDir/{tenant_id}/.
type Store struct {
	RootDir string
}

func (s *Store) tenantDir(tenantID uuid.UUID) string {
	return filepath.Join(s.RootDir, tenantID.String())
}

// Write durably saves one named memory file for tenantID. Called between
// sessions (or by Consolidate) — never mid-session, since a session's
// injected snapshot is already fixed for its own lifetime.
func (s *Store) Write(tenantID uuid.UUID, name string, content []byte) error {
	dir := s.tenantDir(tenantID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("memory: create tenant dir: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("memory: write %s: %w", path, err)
	}
	return nil
}

// fileEntry is one memory file's path plus enough metadata (name, mtime) to
// apply retention without re-reading every file's content up front.
type fileEntry struct {
	name  string
	path  string
	mtime time.Time
}

// list returns tenantID's memory files, oldest first — a missing directory
// (a tenant with no memory yet) is not an error, just an empty list.
func (s *Store) list(tenantID uuid.UUID) ([]fileEntry, error) {
	dir := s.tenantDir(tenantID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: list %s: %w", dir, err)
	}
	var out []fileEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("memory: stat %s: %w", e.Name(), err)
		}
		out = append(out, fileEntry{name: e.Name(), path: filepath.Join(dir, e.Name()), mtime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mtime.Before(out[j].mtime) })
	return out, nil
}
