package oversight_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
)

func newInputs() (*oversight.Inputs, *fakeStore) {
	f := newFakeStore()
	return oversight.NewInputs(f), f
}

// seedInput stores a pending input request and returns it.
func (f *fakeStore) seedInput(sessionID uuid.UUID, onExpiry oversight.OnExpiry, expired bool) oversight.InputRequest {
	ir := oversight.InputRequest{
		InputRequestID: uuid.New(), TenantID: tenant, SessionID: sessionID, Question: "which region?",
		OnExpiry: onExpiry, DefaultAssumption: json.RawMessage(`"eu-west-1"`), Status: oversight.InputPending, ExpiresAt: future,
	}
	if expired {
		ir.ExpiresAt = past
	}
	f.inputs[ir.InputRequestID] = ir
	return ir
}

func TestRequestInput_RecordsTheRequestAndItsEventAndDefaultsToExpire(t *testing.T) {
	in, f := newInputs()
	session := uuid.New()

	ir, err := in.RequestInput(context.Background(), oversight.RequestInputParams{TenantID: tenant, SessionID: session, Question: "which region?"})
	if err != nil {
		t.Fatalf("RequestInput: %v", err)
	}
	if ir.OnExpiry != oversight.OnExpiryExpire || ir.Status != oversight.InputPending {
		t.Errorf("request = on_expiry %q status %q, want expire/pending: an unanswered question must not silently become an assumption", ir.OnExpiry, ir.Status)
	}
	if _, ok := f.inputs[ir.InputRequestID]; !ok {
		t.Error("the request was not stored")
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventInputRequested {
		t.Errorf("events = %v, want input_requested", types)
	}
}

func TestAnswer_RecordsTheAnswerAndItsEvent(t *testing.T) {
	in, f := newInputs()
	seeded := f.seedInput(uuid.New(), oversight.OnExpiryExpire, false)

	got, err := in.Answer(context.Background(), tenant, seeded.InputRequestID, json.RawMessage(`"us-east-1"`))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got.Status != oversight.InputAnswered || string(f.inputs[seeded.InputRequestID].Answer) != `"us-east-1"` || f.inputs[seeded.InputRequestID].UsedDefault {
		t.Errorf("request = %s answer %s, want answered with the human's answer and no default used", got.Status, f.inputs[seeded.InputRequestID].Answer)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventInputAnswered {
		t.Errorf("events = %v, want input_answered", types)
	}
}

func TestAnswer_ARequestThatIsNoLongerPendingCannotBeAnswered(t *testing.T) {
	in, f := newInputs()
	seeded := f.seedInput(uuid.New(), oversight.OnExpiryExpire, false)
	ctx := context.Background()
	if _, err := in.Answer(ctx, tenant, seeded.InputRequestID, json.RawMessage(`"first"`)); err != nil {
		t.Fatalf("first Answer: %v", err)
	}
	if _, err := in.Answer(ctx, tenant, seeded.InputRequestID, json.RawMessage(`"second"`)); err == nil {
		t.Error("a second answer to the same request succeeded")
	}
	if string(f.inputs[seeded.InputRequestID].Answer) != `"first"` {
		t.Errorf("answer = %s, want the first one kept", f.inputs[seeded.InputRequestID].Answer)
	}
}

func TestAnswer_AfterTheTTLIsRefusedAndTheRequestResolvedPerItsPolicy(t *testing.T) {
	// A late answer is refused, but the request's own on_expiry policy still
	// takes effect: refusing must not roll back the typed expiry (or the
	// recorded default assumption) that the lapse owes the run.
	t.Run("on_expiry=expire keeps the typed expiry", func(t *testing.T) {
		in, f := newInputs()
		seeded := f.seedInput(uuid.New(), oversight.OnExpiryExpire, true)
		if _, err := in.Answer(context.Background(), tenant, seeded.InputRequestID, json.RawMessage(`"late"`)); err == nil {
			t.Fatal("an answer after the request's TTL was accepted")
		}
		if got := f.inputs[seeded.InputRequestID]; got.Status != oversight.InputExpired || got.Answer != nil {
			t.Errorf("stored request = %s answer %s, want expired with the late answer discarded", got.Status, got.Answer)
		}
		if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventInputExpired {
			t.Errorf("events = %v, want exactly input_expired", types)
		}
	})
	t.Run("on_expiry=default keeps the recorded assumption, not the late answer", func(t *testing.T) {
		in, f := newInputs()
		seeded := f.seedInput(uuid.New(), oversight.OnExpiryDefault, true)
		if _, err := in.Answer(context.Background(), tenant, seeded.InputRequestID, json.RawMessage(`"late"`)); err == nil {
			t.Fatal("an answer after the request's TTL was accepted")
		}
		got := f.inputs[seeded.InputRequestID]
		if got.Status != oversight.InputAnswered || !got.UsedDefault || string(got.Answer) != `"eu-west-1"` {
			t.Errorf("stored request = %s answer %s default=%v, want answered with the default assumption", got.Status, got.Answer, got.UsedDefault)
		}
		if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventInputAnswered {
			t.Errorf("events = %v, want exactly input_answered", types)
		}
	})
}

func TestExpire_ResolvesAnOverdueRequestPerItsOwnPolicy(t *testing.T) {
	t.Run("on_expiry=expire records a typed expiry", func(t *testing.T) {
		in, f := newInputs()
		seeded := f.seedInput(uuid.New(), oversight.OnExpiryExpire, true)
		got, err := in.Expire(context.Background(), tenant, seeded.InputRequestID)
		if err != nil || got.Status != oversight.InputExpired {
			t.Fatalf("Expire = %s, %v, want expired", got.Status, err)
		}
		if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventInputExpired {
			t.Errorf("events = %v, want input_expired", types)
		}
	})
	t.Run("on_expiry=default answers with the recorded assumption", func(t *testing.T) {
		in, f := newInputs()
		seeded := f.seedInput(uuid.New(), oversight.OnExpiryDefault, true)
		got, err := in.Expire(context.Background(), tenant, seeded.InputRequestID)
		if err != nil || got.Status != oversight.InputAnswered || !got.UsedDefault || string(got.Answer) != `"eu-west-1"` {
			t.Fatalf("Expire = %s answer %s default=%v (%v), want answered with the default assumption", got.Status, got.Answer, got.UsedDefault, err)
		}
		if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventInputAnswered {
			t.Errorf("events = %v, want input_answered (the model sees an ordinary answer, one nobody typed)", types)
		}
	})
	t.Run("a request that is already resolved is left alone", func(t *testing.T) {
		in, f := newInputs()
		seeded := f.seedInput(uuid.New(), oversight.OnExpiryExpire, true)
		ctx := context.Background()
		if _, err := in.Expire(ctx, tenant, seeded.InputRequestID); err != nil {
			t.Fatalf("first Expire: %v", err)
		}
		if _, err := in.Expire(ctx, tenant, seeded.InputRequestID); err != nil {
			t.Fatalf("second Expire: %v", err)
		}
		if len(f.events) != 1 {
			t.Errorf("%d events after expiring twice, want 1: a periodic sweep must be idempotent", len(f.events))
		}
	})
}

func TestInputsInvalidate_ReleasesEveryPendingRequestAndOnlyThose(t *testing.T) {
	in, f := newInputs()
	session := uuid.New()
	f.seedInput(session, oversight.OnExpiryExpire, false)
	f.seedInput(session, oversight.OnExpiryExpire, false)
	other := f.seedInput(uuid.New(), oversight.OnExpiryExpire, false) // another session's request
	answered := f.seedInput(session, oversight.OnExpiryExpire, false)
	if _, err := in.Answer(context.Background(), tenant, answered.InputRequestID, json.RawMessage(`"done"`)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	f.events = nil

	got, err := in.Invalidate(context.Background(), tenant, session, oversight.InvalidationTerminal)
	if err != nil || len(got) != 2 {
		t.Fatalf("Invalidate = %d requests, %v, want the session's 2 pending ones", len(got), err)
	}
	for _, ir := range got {
		if ir.Status != oversight.InputInvalidated || ir.Reason != string(oversight.InvalidationTerminal) {
			t.Errorf("request %s = %s reason %q, want invalidated with the trigger recorded", ir.InputRequestID, ir.Status, ir.Reason)
		}
	}
	if len(f.events) != 2 {
		t.Errorf("%d events, want one input_invalidated per request", len(f.events))
	}
	if f.inputs[other.InputRequestID].Status != oversight.InputPending || f.inputs[answered.InputRequestID].Status != oversight.InputAnswered {
		t.Error("invalidating one session touched another session's request or an already-answered one")
	}
}
