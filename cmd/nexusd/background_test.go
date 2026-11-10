package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
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
