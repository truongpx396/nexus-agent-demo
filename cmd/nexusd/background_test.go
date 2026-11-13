package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
)

func TestStartPeriodic_StopWaitsForThePassAndIsSafeToRepeat(t *testing.T) {
	var passes, running atomic.Int32
	started := make(chan struct{}, 1)
	stop := startPeriodic(t.Context(), time.Millisecond, func(ctx context.Context) {
		running.Add(1)
		defer running.Add(-1)
		passes.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done() // a pass in flight at stop must be told to wind down, and stop must wait for it
	})

	<-started
	stop()
	if n := running.Load(); n != 0 {
		t.Fatalf("%d pass(es) still running after stop returned", n)
	}
	before := passes.Load()
	stop() // a second stop is a no-op, not a panic on a closed channel
	time.Sleep(20 * time.Millisecond)
	if after := passes.Load(); after != before {
		t.Fatalf("passes went from %d to %d after stop: the loop kept ticking", before, after)
	}
}

// A job that reaches a session which no longer needs a worker has nothing to do,
// which is not a failure to publish or retry.
func TestStaleJob(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"no error":                     {nil, false},
		"a session in the wrong state": {fmt.Errorf("%w: session s is already terminal (completed); nothing to resume", runctl.ErrSessionState), true},
		"wrapped more than once":       {fmt.Errorf("resume: %w", fmt.Errorf("%w: suspended", runctl.ErrSessionState)), true},
		"a missing session":            {fmt.Errorf("%w: no such session", runctl.ErrSessionNotFound), false},
		"an infrastructure failure":    {errors.New("connection refused"), false},
	} {
		if got := staleJob(tc.err); got != tc.want {
			t.Errorf("%s: staleJob(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}
