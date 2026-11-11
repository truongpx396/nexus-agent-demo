package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// openingEvent is one event of the batch a run opens with.
type openingEvent struct {
	typ     eventlog.EventType
	actor   eventlog.Actor
	toolID  *string
	payload any
}

// appendBatch seals each event and writes them all through EventLog.AppendBatch,
// then records them in st.History. Either every event is durable or none is.
func (k *Kernel) appendBatch(ctx context.Context, st *RunState, events []openingEvent) ([]eventlog.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	batch := make([]eventlog.Event, len(events))
	for i, ev := range events {
		plaintext, err := json.Marshal(ev.payload)
		if err != nil {
			return nil, fmt.Errorf("marshal %s payload: %w", ev.typ, err)
		}
		sealed, digest, keyID, err := st.Seal(plaintext)
		if err != nil {
			return nil, fmt.Errorf("seal %s payload: %w", ev.typ, err)
		}
		batch[i] = eventlog.Event{
			EventID: uuid.New(), SessionID: st.SessionID, TenantID: st.TenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
			Type: ev.typ, Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: ev.actor, ToolID: ev.toolID,
		}
	}
	out, err := k.Log.AppendBatch(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("append the run's %d opening events: %w", len(batch), err)
	}
	st.History = append(st.History, out...)
	return out, nil
}
