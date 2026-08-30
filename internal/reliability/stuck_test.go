package reliability

import (
	"testing"

	"github.com/google/uuid"
)

func TestTracker_RepeatedAction(t *testing.T) {
	tr := newTracker(4)
	var last TrackerVerdict
	for i := 0; i < 4; i++ {
		last = tr.Record("platform/shell@v1", []byte(`{"cmd":"ls"}`))
	}
	if !last.Suspected || last.Reason != ReasonRepeatedAction {
		t.Fatalf("got %+v, want a suspected repeated_action verdict", last)
	}
	if last.Terminate {
		t.Fatal("the FIRST suspected trip must be non-terminal (task 6.8)")
	}

	// A second, corroborating window of the same pattern must terminate.
	last = tr.Record("platform/shell@v1", []byte(`{"cmd":"ls"}`))
	if !last.Terminate {
		t.Fatal("a second corroborating trip should recommend termination")
	}
}

func TestTracker_Oscillation(t *testing.T) {
	tr := newTracker(4)
	seq := []string{"a", "b", "a", "b", "a", "b", "a", "b"}
	var last TrackerVerdict
	for _, s := range seq {
		last = tr.Record("platform/file_read@v1", []byte(`{"path":"`+s+`"}`))
	}
	if !last.Suspected || last.Reason != ReasonOscillation {
		t.Fatalf("got %+v, want a suspected oscillation verdict", last)
	}
}

func TestTracker_ProgressNeverTrips(t *testing.T) {
	tr := newTracker(4)
	paths := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for _, p := range paths {
		if v := tr.Record("platform/file_read@v1", []byte(`{"path":"`+p+`"}`)); v.Suspected {
			t.Fatalf("distinct, non-repeating actions must never look stuck: %+v", v)
		}
	}
}

func TestTracker_DifferentReasonDoesNotCorroborate(t *testing.T) {
	tr := newTracker(4)
	// Trip once on repeated_action.
	for i := 0; i < 4; i++ {
		tr.Record("platform/shell@v1", []byte(`{"cmd":"ls"}`))
	}
	// Now trip on oscillation instead — a DIFFERENT reason, so this must be
	// treated as a first trip, not a corroborating second one.
	seq := []string{"x", "y", "x", "y"}
	var last TrackerVerdict
	for _, s := range seq {
		last = tr.Record("platform/file_read@v1", []byte(`{"path":"`+s+`"}`))
	}
	if last.Terminate {
		t.Fatal("a differently-reasoned trip must not inherit the prior trip's corroboration count")
	}
}

func TestRegistry_IsolatesSessions(t *testing.T) {
	reg := NewRegistry(4)
	s1, s2 := uuid.New(), uuid.New()
	for i := 0; i < 4; i++ {
		reg.Record(s1, "platform/shell@v1", []byte(`{"cmd":"ls"}`))
	}
	if v := reg.Record(s2, "platform/shell@v1", []byte(`{"cmd":"pwd"}`)); v.Suspected {
		t.Fatal("session s2's tracker must be independent of s1's")
	}
	reg.Forget(s1)
	reg.mu.Lock()
	_, stillThere := reg.byID[s1]
	reg.mu.Unlock()
	if stillThere {
		t.Fatal("Forget should remove the session's tracker")
	}
}

func TestDigest_DistinguishesToolNameBoundary(t *testing.T) {
	// Guards the doc comment's own claim: ("ab","c") must never collide with
	// ("a","bc").
	d1 := Digest("ab", []byte("c"))
	d2 := Digest("a", []byte("bc"))
	if d1 == d2 {
		t.Fatal("Digest must not collide across a tool-name/input boundary shift")
	}
}
