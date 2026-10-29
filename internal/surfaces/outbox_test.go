package surfaces_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
)

// fakeLedger answers Open from fields and records what Record is told, so
// these tests pin the Outbox's own policy — idempotency, the retry cap, what
// is and is not sent — separately from what the Postgres ledger stores.
type fakeLedger struct {
	delivery surfaces.Delivery
	openErr  error
	recErr   error

	opens    int
	recorded []recorded
}

type recorded struct {
	status surfaces.DeliveryStatus
	reason string
}

func (f *fakeLedger) Open(context.Context, uuid.UUID, uuid.UUID, int64, string, string) (surfaces.Delivery, error) {
	f.opens++
	return f.delivery, f.openErr
}

func (f *fakeLedger) Record(_ context.Context, _, _, _ uuid.UUID, status surfaces.DeliveryStatus, reason string) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.recorded = append(f.recorded, recorded{status, reason})
	return nil
}

type recordingSender struct {
	err   error
	calls int
	got   []byte
}

func (s *recordingSender) Send(_ context.Context, _, _ string, payload []byte) error {
	s.calls++
	s.got = payload
	return s.err
}

func deliver(l *fakeLedger, s *recordingSender) error {
	ob := &surfaces.Outbox{Ledger: l}
	return ob.Deliver(context.Background(), uuid.New(), uuid.New(), 1, "telegram", "chat-42", []byte("hello"), s)
}

func TestDeliver_AFreshDeliveryIsSentOnceAndRecordedDelivered(t *testing.T) {
	l := &fakeLedger{delivery: surfaces.Delivery{ID: uuid.New(), Status: surfaces.DeliveryPending}}
	s := &recordingSender{}
	if err := deliver(l, s); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if s.calls != 1 || string(s.got) != "hello" {
		t.Errorf("sender called %d times with %q, want once with the payload", s.calls, s.got)
	}
	if len(l.recorded) != 1 || l.recorded[0].status != surfaces.DeliveryDelivered || l.recorded[0].reason != "" {
		t.Errorf("recorded %+v, want one delivered outcome with no reason", l.recorded)
	}
}

func TestDeliver_ATerminalDeliveryIsNeverSentAgain(t *testing.T) {
	for _, status := range []surfaces.DeliveryStatus{surfaces.DeliveryDelivered, surfaces.DeliveryFailedPermanent, surfaces.DeliverySuppressed} {
		t.Run(string(status), func(t *testing.T) {
			l := &fakeLedger{delivery: surfaces.Delivery{ID: uuid.New(), Status: status, AttemptCount: 3}}
			s := &recordingSender{}
			if err := deliver(l, s); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if s.calls != 0 {
				t.Errorf("a %s delivery was sent again: a repeat call must be a no-op, never a duplicate send", status)
			}
			if len(l.recorded) != 0 {
				t.Errorf("a %s delivery recorded a new outcome %+v", status, l.recorded)
			}
		})
	}
}

func TestDeliver_AFailureIsRetryableUntilTheCapThenPermanent(t *testing.T) {
	boom := errors.New("provider returned 503")
	cases := []struct {
		attemptsSoFar int
		want          surfaces.DeliveryStatus
	}{
		{0, surfaces.DeliveryFailed},
		{1, surfaces.DeliveryFailed},
		{2, surfaces.DeliveryFailedPermanent}, // this is the third attempt
	}
	for _, tc := range cases {
		l := &fakeLedger{delivery: surfaces.Delivery{ID: uuid.New(), Status: surfaces.DeliveryFailed, AttemptCount: tc.attemptsSoFar}}
		s := &recordingSender{err: boom}
		if err := deliver(l, s); err != nil {
			t.Fatalf("Deliver after %d attempts: %v", tc.attemptsSoFar, err)
		}
		if len(l.recorded) != 1 || l.recorded[0].status != tc.want {
			t.Errorf("after %d prior attempts recorded %+v, want %s", tc.attemptsSoFar, l.recorded, tc.want)
		}
		if l.recorded[0].reason != boom.Error() {
			t.Errorf("recorded reason %q, want the sender's own error", l.recorded[0].reason)
		}
	}
}

func TestDeliver_ARetryAfterAFailureIsAttempted(t *testing.T) {
	l := &fakeLedger{delivery: surfaces.Delivery{ID: uuid.New(), Status: surfaces.DeliveryFailed, AttemptCount: 1}}
	s := &recordingSender{}
	if err := deliver(l, s); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if s.calls != 1 || len(l.recorded) != 1 || l.recorded[0].status != surfaces.DeliveryDelivered {
		t.Errorf("a failed (non-terminal) delivery retried %d times and recorded %+v, want one attempt that succeeds", s.calls, l.recorded)
	}
}

func TestDeliver_LedgerFailuresAreReturnedAndNeverSendUnrecorded(t *testing.T) {
	t.Run("the delivery cannot be opened", func(t *testing.T) {
		l := &fakeLedger{openErr: errors.New("db down")}
		s := &recordingSender{}
		err := deliver(l, s)
		if err == nil || !strings.Contains(err.Error(), "open delivery") {
			t.Errorf("Deliver = %v, want an open-delivery error", err)
		}
		if s.calls != 0 {
			t.Error("a message was sent though its delivery was never durably opened: at-least-once needs the row first")
		}
	})
	t.Run("the outcome cannot be recorded", func(t *testing.T) {
		l := &fakeLedger{delivery: surfaces.Delivery{ID: uuid.New(), Status: surfaces.DeliveryPending}, recErr: errors.New("db down")}
		err := deliver(l, &recordingSender{})
		if err == nil || !strings.Contains(err.Error(), "record delivery outcome") {
			t.Errorf("Deliver = %v, want a record-outcome error", err)
		}
	})
}
