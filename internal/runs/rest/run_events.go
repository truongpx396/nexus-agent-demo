package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// eventDTO is what an SSE frame's data carries: enough structure to render
// a client-side view, plus Body — the decrypted payload for the run's own
// audience (constitution: "audience-gated run output" is a distinct signal
// class from content-free telemetry; internal/obs never carries this).
// EventThought never gets a Body: reasoning is "round-tripped, never shown"
// (internal/provider's doc comment) even to the run's own audience.
type eventDTO struct {
	EventID   string          `json:"event_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor"`
	ToolID    *string         `json:"tool_id,omitempty"`
	PairRef   *string         `json:"pair_ref,omitempty"`
	ModelID   *string         `json:"model_id,omitempty"`
	CreatedAt string          `json:"created_at"`
	Body      json.RawMessage `json:"body,omitempty"`
}

func (h *Handler) handleEvents(c *gin.Context) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid run id", http.StatusBadRequest)
		return
	}
	if _, ok := h.ownedRun(c, tenantID, userID, id); !ok {
		return
	}

	// c.Writer is always a Flusher (gin.ResponseWriter embeds http.Flusher), so
	// there is no "streaming unsupported" case to refuse here.
	w, ctx := c.Writer, c.Request.Context()
	c.Header("content-type", "text/event-stream")
	c.Header("cache-control", "no-cache")
	c.Header("connection", "keep-alive")
	c.Status(http.StatusOK)

	// Subscribe BEFORE replaying history, so nothing published between the
	// subscribe call and the replay finishing is missed. That ordering, on
	// its own, can instead deliver an event TWICE (once via the replay,
	// once live) if it's appended in the window between subscribing and
	// finishing the replay — closed below by tracking the highest seq the
	// replay actually sent and discarding anything from the live channel at
	// or below it: seq is strictly sequential per session (store.Append),
	// so "already replayed" is exactly "seq <= lastSeq", no gaps possible.
	ch, unsubscribe := h.broker.subscribe(id)
	defer unsubscribe()

	opener := h.Reader.EventOpener(tenantID)
	write := func(e eventlog.Event) bool {
		dto, derr := toEventDTO(ctx, opener, e)
		if derr != nil {
			writeSSEFrame(w, "error", map[string]string{"error": derr.Error()})
			w.Flush()
			return false
		}
		writeSSEFrame(w, string(e.Type), dto)
		w.Flush()
		return e.Type != eventlog.EventTerminal
	}

	history, err := h.Reader.Events(ctx, tenantID, id)
	if err != nil {
		writeSSEFrame(w, "error", map[string]string{"error": err.Error()})
		w.Flush()
		return
	}
	var lastSeq int64
	for _, e := range history {
		if !write(e) {
			return
		}
		lastSeq = e.Seq
	}

	for {
		select {
		case <-ctx.Done():
			return
		case p, chOK := <-ch:
			if !chOK {
				return
			}
			if p.Delta != nil {
				// Never replayed, never seq-checked against lastSeq — a
				// live preview has no seq at all (DeltaDTO's own doc
				// comment). The durable event it previews still arrives
				// right behind it on this same channel.
				writeSSEFrame(w, "delta", p.Delta)
				w.Flush()
				continue
			}
			if p.Err == nil && p.Event.Seq <= lastSeq {
				continue // already delivered by the historical replay above
			}
			if p.Err != nil {
				writeSSEFrame(w, "error", map[string]string{"error": p.Err.Error()})
				w.Flush()
				return
			}
			if !write(p.Event) {
				return
			}
		}
	}
}

// toEventDTO decrypts one event's payload for display to the run's own
// audience (the principal check in handleEvents has already established this
// request IS that audience). The opener is per-request, so a key unwrapped for
// one event is reused for the rest of the same session.
func toEventDTO(ctx context.Context, opener runs.EventOpener, e eventlog.Event) (eventDTO, error) {
	dto := eventDTO{
		EventID:   e.EventID.String(),
		Seq:       e.Seq,
		Type:      string(e.Type),
		Actor:     string(e.Actor),
		ToolID:    e.ToolID,
		ModelID:   e.ModelID,
		CreatedAt: e.CreatedAt.Format(time.RFC3339Nano),
	}
	if e.PairRef != nil {
		ref := e.PairRef.String()
		dto.PairRef = &ref
	}
	if e.Type == eventlog.EventThought {
		return dto, nil // never shown, even to the run's own audience
	}

	plaintext, err := opener.Open(ctx, e)
	if err != nil {
		return eventDTO{}, err
	}
	dto.Body = json.RawMessage(plaintext)
	return dto, nil
}

func writeSSEFrame(w io.Writer, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{"error":"failed to marshal event"}`)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw) // best-effort: a broken client connection is discovered on the next Flush, not here
}
