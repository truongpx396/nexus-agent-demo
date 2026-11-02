package main

import (
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

// seedSeq yields n EventToolLoaded events numbered 1..n, then err (when
// non-nil) the way Kernel.Seed reports a failed write.
func seedSeq(n int, err error) iter.Seq2[eventlog.Event, error] {
	return func(yield func(eventlog.Event, error) bool) {
		for i := 1; i <= n; i++ {
			if !yield(eventlog.Event{Seq: int64(i), Type: eventlog.EventToolLoaded}, nil) {
				return
			}
		}
		if err != nil {
			yield(eventlog.Event{}, err)
		}
	}
}

// drainWithin runs drainSeed and fails the test, instead of hanging it, if
// the call blocks — which is what a too-small channel buffer does.
func drainWithin(t *testing.T, seq iter.Seq2[eventlog.Event, error]) (chan runs.Event, bool) {
	t.Helper()
	type result struct {
		ch     chan runs.Event
		failed bool
	}
	done := make(chan result, 1)
	go func() {
		ch, failed := drainSeed(seq)
		done <- result{ch, failed}
	}()
	select {
	case r := <-done:
		return r.ch, r.failed
	case <-time.After(5 * time.Second):
		t.Fatal("drainSeed blocked: the channel is smaller than the seed, so POST /v1/runs would hang")
		return nil, false
	}
}

// The default catalog seeds 15 events and the buffer used to hold 8, so the
// ninth send blocked forever. 40 is well past any catalog.
func TestDrainSeed_HoldsMoreEventsThanTheOldFixedBuffer(t *testing.T) {
	const n = 40
	ch, failed := drainWithin(t, seedSeq(n, nil))
	if failed {
		t.Fatal("failed = true for a seed that ended cleanly")
	}
	if got := len(ch); got != n {
		t.Fatalf("buffered %d events, want %d", got, n)
	}
	if free := cap(ch) - len(ch); free < seedHeadroom {
		t.Fatalf("headroom after the seed = %d, want at least %d", free, seedHeadroom)
	}
	close(ch)
	want := int64(1)
	for re := range ch {
		if re.Err != nil || re.Event.Seq != want {
			t.Fatalf("event %d: got seq %d err %v, want seq %d and no error", want, re.Event.Seq, re.Err, want)
		}
		want++
	}
}

func TestDrainSeed_StopsAtTheFirstError(t *testing.T) {
	boom := errors.New("append failed")
	ch, failed := drainWithin(t, seedSeq(3, boom))
	if !failed {
		t.Fatal("failed = false for a seed that ended in an error")
	}
	if got := len(ch); got != 4 {
		t.Fatalf("buffered %d events, want the 3 seeded plus the error", got)
	}
	close(ch)
	var last runs.Event
	for re := range ch {
		last = re
	}
	if !errors.Is(last.Err, boom) {
		t.Fatalf("last event err = %v, want the seed's error", last.Err)
	}
}
