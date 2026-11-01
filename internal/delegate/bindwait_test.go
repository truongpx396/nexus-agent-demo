package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAwaitBound_GivesUpWhenTheDelegationIsNeverBound(t *testing.T) {
	// The race itself is pinned against real Postgres in
	// tests/integration/delegate_bind_race_test.go; here only the deadline: a
	// delegation that is never bound must not wait for ever.
	d := &Delegations{Store: neverBound{}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	// Run it on its own goroutine so that a wait which ignores the deadline
	// fails this test in seconds instead of hanging it until the test binary's
	// own timeout.
	done := make(chan error, 1)
	go func() { done <- d.awaitBound(ctx, uuid.New(), uuid.New()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("awaitBound returned nil for a delegation that was never bound")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitBound ignored the context deadline and kept waiting")
	}
}

type neverBound struct{}

func (neverBound) InTenant(ctx context.Context, _ uuid.UUID, fn func(context.Context, Tx) error) error {
	return fn(ctx, unboundTx{})
}

type unboundTx struct{ Tx }

func (unboundTx) Resolvable(context.Context, uuid.UUID) (bool, error) { return false, nil }
