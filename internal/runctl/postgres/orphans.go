package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
)

// ListRunning returns every session, in every tenant, that the database still
// says is running — the candidates for orphan recovery (runctl.StartRecovery).
// It is genuinely cross-tenant, so pool must be an administrative connection,
// not the RLS-restricted runtime role; whether a candidate is really orphaned is
// not decided here (a live worker holds the session lock).
func ListRunning(ctx context.Context, pool *pgxpool.Pool) ([]runctl.Orphan, error) {
	rows, err := pool.Query(ctx, `SELECT session_id, tenant_id, session_key FROM sessions WHERE status = 'running'`)
	if err != nil {
		return nil, fmt.Errorf("runctl: list running sessions: %w", err)
	}
	defer rows.Close()
	var out []runctl.Orphan
	for rows.Next() {
		var c runctl.Orphan
		if err := rows.Scan(&c.SessionID, &c.TenantID, &c.SessionKey); err != nil {
			return nil, fmt.Errorf("runctl: scan running session: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
