package oversight_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
)

// fakeResume yields a fixed stream for whichever decision is asked and records
// which one it was.
type fakeResume struct {
	events []eventlog.Event
	err    error // yielded after events
	called string
}

func (f *fakeResume) stream() iter.Seq2[eventlog.Event, error] {
	return func(yield func(eventlog.Event, error) bool) {
		for _, e := range f.events {
			if !yield(e, nil) {
				return
			}
		}
		if f.err != nil {
			yield(eventlog.Event{}, f.err)
		}
	}
}

func (f *fakeResume) Grant(context.Context, uuid.UUID, uuid.UUID, string) iter.Seq2[eventlog.Event, error] {
	f.called = "grant"
	return f.stream()
}
func (f *fakeResume) GrantModified(context.Context, uuid.UUID, uuid.UUID, string, json.RawMessage) iter.Seq2[eventlog.Event, error] {
	f.called = "grant_modified"
	return f.stream()
}
func (f *fakeResume) Deny(context.Context, uuid.UUID, uuid.UUID, string, string) iter.Seq2[eventlog.Event, error] {
	f.called = "deny"
	return f.stream()
}

type fakeSettled struct{ told []uuid.UUID }

func (f *fakeSettled) NotifySettled(_ context.Context, _, sessionID uuid.UUID) {
	f.told = append(f.told, sessionID)
}

func eventsFor(session uuid.UUID, n int) []eventlog.Event {
	out := make([]eventlog.Event, n)
	for i := range out {
		out[i] = eventlog.Event{SessionID: session}
	}
	return out
}

func TestDecisions_EachDecisionResumesWithItsOwnKindAndCountsTheEvents(t *testing.T) {
	session := uuid.New()
	ctx, tenantID, approvalID := context.Background(), uuid.New(), uuid.New()

	for _, tc := range []struct {
		want string
		run  func(d oversight.Decisions) oversight.Outcome
	}{
		{"grant", func(d oversight.Decisions) oversight.Outcome { return d.Grant(ctx, tenantID, approvalID, "alice") }},
		{"grant_modified", func(d oversight.Decisions) oversight.Outcome {
			return d.GrantModified(ctx, tenantID, approvalID, "alice", json.RawMessage(`{"to":"bob"}`))
		}},
		{"deny", func(d oversight.Decisions) oversight.Outcome { return d.Deny(ctx, tenantID, approvalID, "alice", "no") }},
	} {
		src := &fakeResume{events: eventsFor(session, 3)}
		out := tc.run(oversight.Decisions{Resume: src})
		if src.called != tc.want {
			t.Errorf("%s: resumed with %q", tc.want, src.called)
		}
		if out.SessionID != session.String() || out.EventsAppended != 3 || out.Err != "" {
			t.Errorf("%s: outcome = %+v, want session %s with 3 events and no error", tc.want, out, session)
		}
	}
}

func TestDecisions_TellsTheObserversOnlyWhenTheResumeSucceeded(t *testing.T) {
	session := uuid.New()
	ctx, tenantID, approvalID := context.Background(), uuid.New(), uuid.New()

	ok := &fakeSettled{}
	oversight.Decisions{Resume: &fakeResume{events: eventsFor(session, 2)}, Settled: ok}.Grant(ctx, tenantID, approvalID, "alice")
	if len(ok.told) != 1 || ok.told[0] != session {
		t.Errorf("a clean resume told %v, want exactly the resumed session", ok.told)
	}

	failed := &fakeSettled{}
	out := oversight.Decisions{Resume: &fakeResume{events: eventsFor(session, 1), err: errors.New("provider down")}, Settled: failed}.Grant(ctx, tenantID, approvalID, "alice")
	if out.Err != "provider down" {
		t.Errorf("Err = %q, want the run's own error", out.Err)
	}
	if len(failed.told) != 0 {
		t.Errorf("a failed resume told %v: it settled nothing", failed.told)
	}

	empty := &fakeSettled{}
	oversight.Decisions{Resume: &fakeResume{}, Settled: empty}.Deny(ctx, tenantID, approvalID, "alice", "no")
	if len(empty.told) != 0 {
		t.Errorf("a resume that produced no events told %v: there is no session to name", empty.told)
	}
}

func TestDecisions_NilSettledIsValid(t *testing.T) {
	out := oversight.Decisions{Resume: &fakeResume{events: eventsFor(uuid.New(), 1)}}.Grant(context.Background(), uuid.New(), uuid.New(), "alice")
	if out.EventsAppended != 1 {
		t.Errorf("outcome = %+v, want the one event counted", out)
	}
}
