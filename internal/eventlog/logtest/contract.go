// Package logtest is the contract suite every kernel.EventLog adapter must
// pass. An adapter's own test builds a Subject and calls Run; because the
// in-memory adapter and the Postgres adapter run the identical suite, a unit
// test that passes against one cannot be relying on behavior the other lacks.
package logtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Subject is the adapter under test plus the read side the port deliberately
// does not expose: the kernel only writes, so each adapter's test supplies
// its own way to look at what was written.
type Subject struct {
	Log kernel.EventLog

	// NewSession registers a session the log may append to (a row, for a
	// database adapter) and returns its identifiers. Every call returns a
	// distinct session.
	NewSession func(t *testing.T) (tenantID, sessionID uuid.UUID)

	// Events returns sessionID's events in sequence order, as persisted.
	Events func(t *testing.T, tenantID, sessionID uuid.UUID) []eventlog.Event

	// Status returns sessionID's status projection as persisted.
	Status func(t *testing.T, tenantID, sessionID uuid.UUID) (status string, terminalReason *string)
}

func event(tenantID, sessionID uuid.UUID, typ eventlog.EventType, payload string) eventlog.Event {
	return eventlog.Event{
		EventID:       uuid.New(),
		SessionID:     sessionID,
		TenantID:      tenantID,
		SchemaVersion: eventlog.CurrentSchemaVersion,
		Type:          typ,
		Payload:       []byte(payload),
		PayloadDigest: []byte("digest-" + payload),
		KeyID:         "key-1",
		Actor:         eventlog.ActorSystem,
	}
}

// Run executes the contract against s.
func Run(t *testing.T, s Subject) {
	ctx := context.Background()

	t.Run("Append numbers a session's events from 1 and ignores the caller's Seq", func(t *testing.T) {
		tenantID, sessionID := s.NewSession(t)
		for i, typ := range []eventlog.EventType{eventlog.EventUserMessage, eventlog.EventContent, eventlog.EventTerminal} {
			e := event(tenantID, sessionID, typ, "p")
			e.Seq = 99 // must be ignored
			got, err := s.Log.Append(ctx, e)
			if err != nil {
				t.Fatalf("Append #%d: %v", i+1, err)
			}
			if got.Seq != int64(i+1) {
				t.Fatalf("Append #%d returned Seq %d, want %d", i+1, got.Seq, i+1)
			}
			if got.CreatedAt.IsZero() {
				t.Fatalf("Append #%d returned a zero CreatedAt", i+1)
			}
		}
	})

	t.Run("Append stores exactly what it was given", func(t *testing.T) {
		tenantID, sessionID := s.NewSession(t)
		toolID, modelID := "platform/echo@v1", "fake"
		pair := uuid.New()
		in := event(tenantID, sessionID, eventlog.EventToolResult, "sealed-bytes")
		in.Actor, in.ToolID, in.ModelID, in.PairRef = eventlog.ActorTool, &toolID, &modelID, &pair

		got, err := s.Log.Append(ctx, in)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		stored := s.Events(t, tenantID, sessionID)
		if len(stored) != 1 {
			t.Fatalf("persisted %d events, want 1", len(stored))
		}
		for name, e := range map[string]eventlog.Event{"returned": got, "persisted": stored[0]} {
			if e.EventID != in.EventID || e.Type != in.Type || e.Actor != in.Actor || e.KeyID != in.KeyID ||
				e.SchemaVersion != in.SchemaVersion || string(e.Payload) != string(in.Payload) ||
				string(e.PayloadDigest) != string(in.PayloadDigest) {
				t.Errorf("%s event %+v does not carry the appended fields %+v", name, e, in)
			}
			if e.ToolID == nil || *e.ToolID != toolID || e.ModelID == nil || *e.ModelID != modelID ||
				e.PairRef == nil || *e.PairRef != pair {
				t.Errorf("%s event lost an optional field: tool=%v model=%v pair=%v", name, e.ToolID, e.ModelID, e.PairRef)
			}
		}
	})

	t.Run("sessions have independent sequences", func(t *testing.T) {
		tenantID, a := s.NewSession(t)
		_, b := s.NewSession(t)
		for _, sid := range []uuid.UUID{a, a, b} {
			if _, err := s.Log.Append(ctx, event(tenantID, sid, eventlog.EventContent, "x")); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		if got := len(s.Events(t, tenantID, a)); got != 2 {
			t.Errorf("session a has %d events, want 2", got)
		}
		eb := s.Events(t, tenantID, b)
		if len(eb) != 1 || eb[0].Seq != 1 {
			t.Errorf("session b events = %+v, want exactly one with Seq 1", eb)
		}
	})

	t.Run("SetStatus is a last-write-wins projection", func(t *testing.T) {
		tenantID, sessionID := s.NewSession(t)
		if err := s.Log.SetStatus(ctx, tenantID, sessionID, eventlog.SessionStatusRunning, nil); err != nil {
			t.Fatalf("SetStatus running: %v", err)
		}
		if st, reason := s.Status(t, tenantID, sessionID); st != eventlog.SessionStatusRunning || reason != nil {
			t.Fatalf("status = %q reason=%v, want running/nil", st, reason)
		}
		why := "completed"
		if err := s.Log.SetStatus(ctx, tenantID, sessionID, eventlog.SessionStatusCompleted, &why); err != nil {
			t.Fatalf("SetStatus completed: %v", err)
		}
		if st, reason := s.Status(t, tenantID, sessionID); st != eventlog.SessionStatusCompleted || reason == nil || *reason != why {
			t.Fatalf("status = %q reason=%v, want completed/%q", st, reason, why)
		}
	})

	t.Run("RecordSuspension and RecordDelegation accept a well-formed request", func(t *testing.T) {
		tenantID, sessionID := s.NewSession(t)
		if err := s.Log.RecordSuspension(ctx, kernel.SuspendRequest{
			TenantID: tenantID, SessionID: sessionID, ToolUseEventID: uuid.New(), ApprovalEventID: uuid.New(),
			ToolID: "platform/shell@v1", AskKind: "once",
		}); err != nil {
			t.Errorf("RecordSuspension: %v", err)
		}
		if err := s.Log.RecordDelegation(ctx, kernel.DelegateSuspendRequest{
			TenantID: tenantID, SessionID: sessionID, ToolUseEventID: uuid.New(), DelegationEventID: uuid.New(),
			ToolID: "platform/delegate@v1", ChildSessionID: uuid.New(),
		}); err != nil {
			t.Errorf("RecordDelegation: %v", err)
		}
	})
}
