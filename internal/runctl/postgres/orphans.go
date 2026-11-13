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
//
// A session with a claim still in flight is left out. The claim means a tool's
// effect may have left the process and nobody has said whether it landed (task
// 6.6), which only a person or a probe can resolve (runctl.ResolveClaim);
// re-enqueueing the session does not resolve it and the resume would only refuse
// again, so a recurring sweep would re-enqueue it for ever. Once the claim is
// resolved the session is a candidate again and the next sweep picks it up.
func ListRunning(ctx context.Context, pool *pgxpool.Pool) ([]runctl.Orphan, error) {
	rows, err := pool.Query(ctx, `
		SELECT s.session_id, s.tenant_id, s.session_key
		FROM sessions s
		WHERE s.status = 'running'
		  AND NOT EXISTS (SELECT 1 FROM claims c WHERE c.session_id = s.session_id AND c.status = $1)`,
		string(runctl.ClaimInFlight))
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
