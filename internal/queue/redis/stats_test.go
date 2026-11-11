package redis

import (
	"testing"
	"time"
)

func TestAgeOfEntry(t *testing.T) {
	now := time.UnixMilli(1_700_000_010_000)
	for name, tc := range map[string]struct {
		id   string
		want time.Duration
	}{
		"ten seconds old":     {"1700000000000-0", 10 * time.Second},
		"sequence is ignored": {"1700000000000-7", 10 * time.Second},
		"from the future":     {"1700000020000-0", 0},
		"not an id":           {"nonsense", 0},
		"empty":               {"", 0},
	} {
		if got := ageOfEntry(tc.id, now); got != tc.want {
			t.Errorf("%s: ageOfEntry(%q) = %v, want %v", name, tc.id, got, tc.want)
		}
	}
}
