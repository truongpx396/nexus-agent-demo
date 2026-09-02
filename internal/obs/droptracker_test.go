package obs

import "testing"

func TestDropTrackerRateZeroWithNoAttempts(t *testing.T) {
	tr := NewDropTracker()
	if got := tr.Rate(); got != 0 {
		t.Fatalf("Rate() with no attempts = %v, want 0", got)
	}
}

func TestFilterTrackedCountsAttemptedAndDropped(t *testing.T) {
	tr := NewDropTracker()
	FilterTracked(Attrs{"tool.id": "x", "conversation.content": "secret"}, tr) // 1 allowed, 1 dropped
	FilterTracked(Attrs{"terminal_reason": "completed"}, tr)                   // 1 allowed, 0 dropped

	got := tr.Rate()
	want := 1.0 / 3.0
	if got < want-0.0001 || got > want+0.0001 {
		t.Fatalf("Rate() = %v, want ~%v (1 dropped of 3 attempted)", got, want)
	}
}

func TestFilterTrackedNilTrackerIsNoOp(t *testing.T) {
	out := FilterTracked(Attrs{"tool.id": "x", "secret": "y"}, nil)
	if _, ok := out["tool.id"]; !ok {
		t.Fatal("FilterTracked(nil tracker) should still filter normally")
	}
	if _, ok := out["secret"]; ok {
		t.Fatal("FilterTracked(nil tracker) let an unlisted key through")
	}
}

func TestDropTrackerReset(t *testing.T) {
	tr := NewDropTracker()
	FilterTracked(Attrs{"secret": "y"}, tr)
	if tr.Rate() == 0 {
		t.Fatal("expected a nonzero rate before Reset")
	}
	tr.Reset()
	if got := tr.Rate(); got != 0 {
		t.Fatalf("Rate() after Reset = %v, want 0", got)
	}
}
