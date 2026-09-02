package evals

import "testing"

func TestWilsonIntervalZeroTrialsIsMaximallyUncertain(t *testing.T) {
	iv := WilsonInterval(0, 0, DefaultZ)
	if iv.Low != 0 || iv.High != 1 {
		t.Fatalf("WilsonInterval(0,0) = %+v, want [0,1]", iv)
	}
}

func TestWilsonIntervalAllPassNarrowsTowardOneAsNGrows(t *testing.T) {
	small := WilsonInterval(5, 5, DefaultZ)
	large := WilsonInterval(500, 500, DefaultZ)
	if !(large.Low > small.Low) {
		t.Fatalf("large-n all-pass interval (%v) should have a higher lower bound than small-n (%v)", large, small)
	}
	if large.High < 0.999999 {
		t.Fatalf("all-pass interval high bound = %v, want ~1 (within float rounding)", large.High)
	}
}

func TestWilsonIntervalAllFailIsZeroToSomething(t *testing.T) {
	iv := WilsonInterval(0, 10, DefaultZ)
	if iv.Low != 0 {
		t.Fatalf("all-fail interval low = %v, want 0", iv.Low)
	}
	if iv.High <= 0 || iv.High >= 0.5 {
		t.Fatalf("all-fail interval high = %v, want a small positive number", iv.High)
	}
}

func TestIntervalSeparated(t *testing.T) {
	low := Interval{Low: 0.1, High: 0.3}
	high := Interval{Low: 0.6, High: 0.9}
	overlapping := Interval{Low: 0.25, High: 0.65}

	if !low.Separated(high) {
		t.Fatal("low should be Separated below high")
	}
	if high.Separated(low) {
		t.Fatal("high should NOT be Separated below low (it's the one on top)")
	}
	if low.Separated(overlapping) || overlapping.Separated(low) {
		t.Fatal("overlapping intervals must never report Separated in either direction")
	}
}

func TestRunKTrialsRunsExactlyK(t *testing.T) {
	calls := 0
	trials := RunKTrials(4, func(int) Trial {
		calls++
		return Trial{Verdict: VerdictPass}
	})
	if calls != 4 || len(trials) != 4 {
		t.Fatalf("RunKTrials(4, ...) ran %d times, produced %d trials, want 4/4", calls, len(trials))
	}
}

func TestRunKTrialsClampsBelowOne(t *testing.T) {
	trials := RunKTrials(0, func(int) Trial { return Trial{Verdict: VerdictPass} })
	if len(trials) != 1 {
		t.Fatalf("RunKTrials(0, ...) produced %d trials, want 1 (clamped)", len(trials))
	}
}
