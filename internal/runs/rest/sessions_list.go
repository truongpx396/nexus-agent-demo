package rest

import (
	"net/http"

	"github.com/gin-gonic/gin"

	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// listSessionsLimit bounds GET /v1/sessions -- a sidebar has no reason to
// render more than this many threads; older ones are still reachable
// directly via GET /v1/runs/{id} if a caller has the id (e.g. from a
// previous listing, or the browser's own recentRuns cache).
const listSessionsLimit = 200

// ListSessions is GET /v1/sessions: the calling principal's own root sessions,
// newest first -- the web UI's session sidebar. SessionSummary is deliberately
// smaller than Run (no forked_from/plan/team columns): a sidebar needs just
// enough to render and sort a session list, not the full row.
func (h *Handler) ListSessions(c *gin.Context) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}

	runList, err := h.Reader.ListRuns(c.Request.Context(), tenantID, userID, listSessionsLimit)
	if err != nil {
		restkit.ServerError(c, "list sessions", err)
		return
	}

	views := make([]SessionSummary, len(runList))
	for i, run := range runList {
		views[i] = SessionSummary{
			RunID: run.RunID, Status: run.Status, TerminalReason: run.TerminalReason,
			CreatedAt: run.CreatedAt, AutonomyLevel: run.AutonomyLevel,
		}
	}
	c.JSON(http.StatusOK, views)
}
