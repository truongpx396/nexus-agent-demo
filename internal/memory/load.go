package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Snapshot is what gets folded into a fresh session's byte-stable system
// prompt (kernel.RunConfig.System) — Text is the concatenated, screened
// content; SourceIDs is the audit record kernel.RunConfig.MemorySources
// carries into one EventMemoryLoaded event, mirroring EventToolLoaded's own
// "record what was pinned, don't re-inject it as a message" split.
type Snapshot struct {
	Text      string
	SourceIDs []string
}

// Load builds tenantID's memory snapshot: retention-filtered (config.Load's
// MemoryRetentionDays, mtime-based), screened (Screen, fail-closed — a
// flagged or rejected file is skipped and logged, never injected), and
// concatenated under per-file headers. Called once per fresh session
// (cmd/nexusd's kernelRunStarter.StartRun) — "writes take effect next
// session" is exactly the fact that Load runs at session start and never
// again mid-session.
func (s *Store) Load(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (Snapshot, error) {
	cfg, err := config.Load(ctx, tx, tenantID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("memory: load tenant config: %w", err)
	}
	entries, err := s.list(tenantID)
	if err != nil {
		return Snapshot{}, err
	}

	cutoff := time.Now().AddDate(0, 0, -cfg.MemoryRetentionDays)
	var buf strings.Builder
	var sources []string
	for _, e := range entries {
		if e.mtime.Before(cutoff) {
			continue // past retention — never injected, but left on disk for an operator to inspect/purge
		}
		content, err := os.ReadFile(e.path) //nolint:gosec // e.path is built from this tenant's own memory dir, never request input
		if err != nil {
			return Snapshot{}, fmt.Errorf("memory: read %s: %w", e.path, err)
		}
		status, findings := Screen(string(content))
		if status != StatusClean {
			slog.Warn("memory: skipped a memory file that failed screening", "tenant_id", tenantID, "file", e.name, "status", status, "findings", findings)
			continue
		}
		fmt.Fprintf(&buf, "# memory: %s\n%s\n\n", e.name, content)
		sources = append(sources, e.name)
	}
	return Snapshot{Text: buf.String(), SourceIDs: sources}, nil
}

// LoadForSession is Load's convenience wrapper for a caller that only has a
// *store.Store, not an open transaction — cmd/nexusd's kernelRunStarter is
// the one caller, at the moment it builds a fresh session's RunConfig
// (README task 7.1's "injected at session start").
func (s *Store) LoadForSession(ctx context.Context, st *store.Store, tenantID uuid.UUID) (Snapshot, error) {
	var snap Snapshot
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snap, err = s.Load(ctx, tx, tenantID)
		return err
	})
	return snap, err
}
