//go:build integration

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// countingQueue is a queue.Port that only counts Enqueue calls — what the
// race below needs to see is how many continuation jobs got queued, not
// anything a worker would then do with them.
type countingQueue struct{ enqueued atomic.Int32 }

func (q *countingQueue) Enqueue(_ context.Context, job queue.Job) (queue.Job, error) {
	q.enqueued.Add(1)
	return job, nil
}
func (q *countingQueue) Lease(context.Context, string, time.Duration) (queue.Job, bool, error) {
	return queue.Job{}, false, nil
}
func (q *countingQueue) Complete(context.Context, uuid.UUID) error { return nil }
func (q *countingQueue) Fail(context.Context, uuid.UUID, string, bool, time.Time) error {
	return nil
}

// ResumeConversation checks that a session is awaiting_input and then flips
// it to running, appending the human's message in between. If that check
// and the flip aren't one atomic step, two callers racing the same session
// (a REST steer and a webhook delivery, or two deliveries) can both pass the
// check: both append a user_message and both queue a continuation job, so
// the one paused conversation runs two paid turns. Exactly one caller may
// win; the rest must be refused as "not awaiting_input".
func TestRunCtl_ResumeConversation_ConcurrentCallersResumeExactlyOnce(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, sessionID := uuid.New(), uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := keys.NewDEK(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: uuid.New(), AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "test-model",
			AutonomyLevel: "supervised", Conversational: true,
		}); err != nil {
			return err
		}
		// runctl seals under the key of the session's most recent event, so
		// the session needs one — the pause a real conversation ends on.
		payload := []byte(`{}`)
		sealed, err := crypto.Seal(dek, payload, tenantID.String(), sessionID.String())
		if err != nil {
			return err
		}
		if _, err := store.Append(ctx, tx, store.Event{
			EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: store.CurrentSchemaVersion,
			Type: store.EventAwaitingInput, Payload: sealed, PayloadDigest: crypto.Digest(payload), KeyID: dek.KeyID, Actor: store.ActorSystem,
		}); err != nil {
			return err
		}
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusAwaitingInput, nil)
	})
	if err != nil {
		t.Fatalf("set up awaiting_input session: %v", err)
	}

	q := &countingQueue{}
	ctl := &runctl.Control{Store: st, Keys: keys, Queue: q}

	const callers = 8
	// Open every connection up front: the pool otherwise dials them lazily,
	// which staggers the callers enough to hide the race this test is for.
	warm := make([]*pgxpool.Conn, callers)
	for i := range warm {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("warm connection %d: %v", i, err)
		}
		warm[i] = c
	}
	for _, c := range warm {
		c.Release()
	}

	var ok atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := ctl.ResumeConversation(ctx, tenantID, sessionID, "follow-up"); err == nil {
				ok.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := ok.Load(); got != 1 {
		t.Errorf("%d of %d concurrent ResumeConversation calls succeeded, want exactly 1", got, callers)
	}
	if got := q.enqueued.Load(); got != 1 {
		t.Errorf("%d continuation jobs enqueued, want exactly 1 (each extra is a second paid turn)", got)
	}
	events, err := listEventsDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var userMessages int
	for _, e := range events {
		if e.Type == store.EventUserMessage {
			userMessages++
		}
	}
	if userMessages != 1 {
		t.Errorf("%d user_message events appended, want exactly 1", userMessages)
	}
}
