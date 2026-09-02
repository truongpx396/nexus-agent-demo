package obs

import (
	"encoding/json"
	"io"
)

// Span is the minimal shape the exporter below emits. Full turn-scoped span
// emission driven from the event log (one span per turn/plan step, linked
// to its predecessor, carrying the covered seq range) lands in Phase 6/9;
// this type and Exporter exist now so every later phase filters through the
// same allowlist from day one rather than retrofitting it once content-rich
// spans already exist.
type Span struct {
	Name  string `json:"name"`
	Attrs Attrs  `json:"attrs"`
}

// Exporter writes filtered spans as newline-delimited JSON. It is a stand-in
// for the OTLP exporter later phases wire up — the filtering guarantee, not
// the wire format, is what Phase 1 is proving.
type Exporter struct {
	w io.Writer
	// Drops counts every Emit's attempted-vs-dropped attribute keys (Phase
	// 10, README task 10.12's "telemetry attribute-drop rate") — nil is
	// valid and simply means this exporter isn't counted toward any
	// dashboard.
	Drops *DropTracker
}

func NewExporter(w io.Writer) *Exporter {
	return &Exporter{w: w}
}

// Emit filters attrs through the allowlist before writing — the exporter
// itself cannot bypass Filter, because it never sees anything else.
func (e *Exporter) Emit(name string, attrs Attrs) error {
	span := Span{Name: name, Attrs: FilterTracked(attrs, e.Drops)}
	enc := json.NewEncoder(e.w)
	return enc.Encode(span)
}
