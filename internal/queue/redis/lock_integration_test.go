//go:build integration

package redis

import (
	"context"
	"testing"
	"time"
)

// The lock's release and renew are Lua scripts, which are only text to the
// compiler: this is the test that runs them against a real Redis.
func TestSessionLock_AcquireReleaseRenewAndNeverStealing(t *testing.T) {
	_, client := newTestQueue(t)
	lock := NewSessionLock(client, 5*time.Second)
	ctx := context.Background()

	token, ok, err := lock.Acquire(ctx, "session-a")
	if err != nil || !ok || token == "" {
		t.Fatalf("Acquire = %q, %v, %v, want the lock", token, ok, err)
	}
	if _, ok, _ := lock.Acquire(ctx, "session-a"); ok {
		t.Fatal("a second Acquire of a held lock succeeded: two workers would drive one session")
	}
	if _, ok, _ := lock.Acquire(ctx, "session-b"); !ok {
		t.Error("a different session's lock was refused: only the same session is serialised")
	}

	// Another holder's token must neither release nor renew it.
	if err := lock.Release(ctx, "session-a", "someone-elses-token"); err != nil {
		t.Fatalf("Release with the wrong token: %v", err)
	}
	if renewed, err := lock.Renew(ctx, "session-a", "someone-elses-token"); err != nil || renewed {
		t.Errorf("Renew with the wrong token = %v, %v, want false", renewed, err)
	}
	if _, ok, _ := lock.Acquire(ctx, "session-a"); ok {
		t.Fatal("a wrong-token release stole the lock")
	}

	if renewed, err := lock.Renew(ctx, "session-a", token); err != nil || !renewed {
		t.Errorf("Renew with the holder's token = %v, %v, want true", renewed, err)
	}
	if err := lock.Release(ctx, "session-a", token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok, _ := lock.Acquire(ctx, "session-a"); !ok {
		t.Error("the lock was not free after its holder released it")
	}
}

// Held is what the orphan sweep trusts to tell a live run from an abandoned one:
// true from Acquire until Release or expiry, false otherwise.
func TestSessionLock_HeldTracksAcquireReleaseAndExpiry(t *testing.T) {
	_, client := newTestQueue(t)
	lock := NewSessionLock(client, 300*time.Millisecond)
	ctx := context.Background()

	if held, err := lock.Held(ctx, "session-a"); err != nil || held {
		t.Fatalf("Held before any Acquire = %v, %v, want false", held, err)
	}
	token, _, _ := lock.Acquire(ctx, "session-a")
	if held, _ := lock.Held(ctx, "session-a"); !held {
		t.Error("Held = false while the lock is held")
	}
	if held, _ := lock.Held(ctx, "session-b"); held {
		t.Error("Held = true for a different session")
	}
	if err := lock.Release(ctx, "session-a", token); err != nil {
		t.Fatal(err)
	}
	if held, _ := lock.Held(ctx, "session-a"); held {
		t.Error("Held = true after Release")
	}

	// A holder that died stops renewing, so its lock expires on its own.
	if _, ok, _ := lock.Acquire(ctx, "session-c"); !ok {
		t.Fatal("Acquire session-c failed")
	}
	time.Sleep(450 * time.Millisecond)
	if held, _ := lock.Held(ctx, "session-c"); held {
		t.Error("Held = true after the TTL with no renewal: a dead worker's session would never be recovered")
	}
}
