package obs

import "sync/atomic"

// DropTracker counts how many attribute keys Filter saw versus dropped,
// across the life of the tracker — the golden-signal dashboard's (README
// task 10.12) "telemetry attribute-drop rate." This is a deliberate
// simplification: spans themselves are never persisted (Exporter just
// writes NDJSON to whatever io.Writer it's given), so there is no durable
// historical record to query the way every OTHER golden signal in
// dashboard.go is queried straight out of Postgres. A live, process-
// lifetime counter is the honest "S" alternative to inventing a spans
// table this phase doesn't otherwise need — construct one, pass it to
// FilterTracked at every emit site that wants to be counted, and read Rate
// from it whenever a dashboard is assembled.
type DropTracker struct {
	attempted atomic.Int64
	dropped   atomic.Int64
}

func NewDropTracker() *DropTracker { return &DropTracker{} }

// FilterTracked is Filter, instrumented: every call counts how many keys it
// saw and how many it dropped into t. Filter itself is untouched — a caller
// that doesn't care about the drop rate keeps calling Filter directly, and
// t may be nil (a no-op, so call sites don't need a nil check of their
// own).
func FilterTracked(in Attrs, t *DropTracker) Attrs {
	out := Filter(in)
	if t != nil {
		t.attempted.Add(int64(len(in)))
		t.dropped.Add(int64(len(in) - len(out)))
	}
	return out
}

// Rate returns the fraction of attribute keys dropped since the tracker was
// created (or last Reset). Zero attempts reports rate 0, not NaN.
func (t *DropTracker) Rate() float64 {
	attempted := t.attempted.Load()
	if attempted == 0 {
		return 0
	}
	return float64(t.dropped.Load()) / float64(attempted)
}

// Reset zeroes the tracker — a caller that wants "since the last dashboard
// read" rather than "since process start" calls this after reading Rate.
func (t *DropTracker) Reset() {
	t.attempted.Store(0)
	t.dropped.Store(0)
}
