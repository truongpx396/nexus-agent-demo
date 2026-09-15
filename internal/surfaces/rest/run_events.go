package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
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

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	sess, err := s.getSession(r.Context(), tenantID, id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if sess.UserID != userID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE replaying history, so nothing published between the
	// subscribe call and the replay finishing is missed. That ordering, on
	// its own, can instead deliver an event TWICE (once via the replay,
	// once live) if it's appended in the window between subscribing and
	// finishing the replay — closed below by tracking the highest seq the
	// replay actually sent and discarding anything from the live channel at
	// or below it: seq is strictly sequential per session (store.Append),
	// so "already replayed" is exactly "seq <= lastSeq", no gaps possible.
	ch, unsubscribe := s.broker.subscribe(id)
	defer unsubscribe()

	dekCache := map[string]crypto.DEK{}
	write := func(e store.Event) bool {
		dto, derr := s.toEventDTO(r.Context(), tenantID, e, dekCache)
		if derr != nil {
			writeSSEFrame(w, "error", map[string]string{"error": derr.Error()})
			flusher.Flush()
			return false
		}
		writeSSEFrame(w, string(e.Type), dto)
		flusher.Flush()
		return e.Type != store.EventTerminal
	}

	history, err := s.listEvents(r.Context(), tenantID, id)
	if err != nil {
		writeSSEFrame(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
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
		case <-r.Context().Done():
			return
		case p, chOK := <-ch:
			if !chOK {
				return
			}
			if p.Err == nil && p.Event.Seq <= lastSeq {
				continue // already delivered by the historical replay above
			}
			if p.Err != nil {
				writeSSEFrame(w, "error", map[string]string{"error": p.Err.Error()})
				flusher.Flush()
				return
			}
			if !write(p.Event) {
				return
			}
		}
	}
}

func (s *Server) listEvents(ctx context.Context, tenantID, sessionID uuid.UUID) ([]store.Event, error) {
	var events []store.Event
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		events, lerr = store.ListEvents(ctx, tx, sessionID)
		return lerr
	})
	return events, err
}

// toEventDTO decrypts one event's payload for display to the run's own
// audience (the principal check in handleEvents has already established
// this request IS that audience). dekCache avoids re-unwrapping the same
// key (in practice, every event in one session shares it) once per event.
func (s *Server) toEventDTO(ctx context.Context, tenantID uuid.UUID, e store.Event, dekCache map[string]crypto.DEK) (eventDTO, error) {
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
	if e.Type == store.EventThought {
		return dto, nil // never shown, even to the run's own audience
	}

	dek, ok := dekCache[e.KeyID]
	if !ok {
		var uerr error
		err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			dek, uerr = s.KeyStore.Unwrap(ctx, tx, e.KeyID)
			return uerr
		})
		if err != nil {
			return eventDTO{}, fmt.Errorf("unwrap key for event %s: %w", e.EventID, err)
		}
		dekCache[e.KeyID] = dek
	}

	plaintext, err := crypto.Open(dek, e.Payload, tenantID.String(), e.SessionID.String())
	if err != nil {
		return eventDTO{}, fmt.Errorf("decrypt event %s: %w", e.EventID, err)
	}
	dto.Body = json.RawMessage(plaintext)
	return dto, nil
}

func writeSSEFrame(w http.ResponseWriter, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{"error":"failed to marshal event"}`)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw) // best-effort: a broken client connection is discovered on the next Flush, not here
}
