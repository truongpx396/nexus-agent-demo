package rest

import (
	"net/http"
	"time"
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
// sessions, newest first -- the web UI's session sidebar.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}

	runList, err := s.Reader.ListRuns(r.Context(), tenantID, userID, listSessionsLimit)
	if err != nil {
		http.Error(w, "list sessions: "+err.Error(), http.StatusInternalServerError)
		return
	}

	views := make([]sessionSummary, len(runList))
	for i, run := range runList {
		views[i] = sessionSummary{
			RunID: run.RunID.String(), Status: run.Status, TerminalReason: run.TerminalReason,
			CreatedAt: run.CreatedAt, AutonomyLevel: run.AutonomyLevel,
		}
	}
	writeJSON(w, http.StatusOK, views)
}
