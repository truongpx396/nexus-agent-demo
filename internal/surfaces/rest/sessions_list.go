package rest

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// listSessionsLimit bounds GET /v1/sessions -- a sidebar has no reason to
// render more than this many threads; older ones are still reachable
// directly via GET /v1/runs/{id} if a caller has the id (e.g. from a
// previous listing, or the browser's own recentRuns cache).
const listSessionsLimit = 200

// sessionSummary is GET /v1/sessions' own per-thread shape -- deliberately
// smaller than getRunResponse (no forked_from/plan/team columns): a sidebar
// needs just enough to render and sort a session list, not the full row.
type sessionSummary struct {
	RunID          string    `json:"run_id"`
	Status         string    `json:"status"`
	TerminalReason *string   `json:"terminal_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	AutonomyLevel  string    `json:"autonomy_level"`
}

// handleListSessions is GET /v1/sessions: the calling principal's own root
// sessions (internal/store.ListSessionsForUser), newest first -- the web
// UI's session sidebar. No Port indirection needed: this package already
// imports internal/store directly for getSession/listEvents, unlike the
// kernel-adjacent ports (OversightPort, RunCtlPort, ...) that exist
// specifically because internal/runctl/internal/oversight transitively
// import kernel and this package must not.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	var sessions []store.Session
	err := s.Store.InTenantTx(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		sessions, lerr = store.ListSessionsForUser(ctx, tx, userID, listSessionsLimit)
		return lerr
	})
	if err != nil {
		http.Error(w, "list sessions: "+err.Error(), http.StatusInternalServerError)
		return
	}

	views := make([]sessionSummary, len(sessions))
	for i, sess := range sessions {
		views[i] = sessionSummary{
			RunID: sess.SessionID.String(), Status: sess.Status, TerminalReason: sess.TerminalReason,
			CreatedAt: sess.CreatedAt, AutonomyLevel: sess.AutonomyLevel,
		}
	}
	writeJSON(w, http.StatusOK, views)
}
