package store

import (
	"encoding/json"
	"fmt"
)

// Upcaster transforms a payload written under one schema_version into the
// shape the next version expects. FR-086: every appended event carries an
// explicit schema version, and a documented upcasting path keeps events
// written under an older schema replayable after the shape changes —
// without this, "replay a run from years ago" is a claim, not a property.
type Upcaster func(raw json.RawMessage) (json.RawMessage, error)

// upcastRegistry[type][fromVersion] transforms a payload from fromVersion to
// fromVersion+1. Registered here, not scattered at call sites, so the whole
// upcasting path for a type is readable in one place.
var upcastRegistry = map[EventType]map[int]Upcaster{
	// v1 EventContent payload was {"text": "..."}. v2 renamed the field to
	// "body" to match the field name every other content-bearing event
	// taxonomy entry uses. A real (if small) example of the kind of shape
	// drift an append-only log must survive without breaking old replays.
	EventContent: {
		1: upcastContentV1toV2,
	},
}

type contentV1 struct {
	Text string `json:"text"`
}

type contentV2 struct {
	Body string `json:"body"`
}

func upcastContentV1toV2(raw json.RawMessage) (json.RawMessage, error) {
	var v1 contentV1
	if err := json.Unmarshal(raw, &v1); err != nil {
		return nil, fmt.Errorf("upcast content v1->v2: %w", err)
	}
	return json.Marshal(contentV2{Body: v1.Text})
}

// Upcast repeatedly applies the registered chain for (t, version) until no
// further upcaster is registered, returning the resulting payload and the
// version it now conforms to. A type/version with no registered upcaster is
// returned unchanged — the common case, since most events never need one.
func Upcast(t EventType, version int, payload json.RawMessage) (json.RawMessage, int, error) {
	for {
		fn, ok := upcastRegistry[t][version]
		if !ok {
			return payload, version, nil
		}
		next, err := fn(payload)
		if err != nil {
			return nil, version, err
		}
		payload = next
		version++
	}
}
