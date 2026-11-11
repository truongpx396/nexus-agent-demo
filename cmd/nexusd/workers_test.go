package main

import "testing"

func TestQueueWorkers(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		want int
	}{
		"unset":        {"", queueWorkerCount},
		"set":          {"8", 8},
		"the ceiling":  {"64", 64},
		"over":         {"65", queueWorkerCount},
		"zero":         {"0", queueWorkerCount},
		"negative":     {"-3", queueWorkerCount},
		"not a number": {"many", queueWorkerCount},
	} {
		t.Setenv("NEXUS_QUEUE_WORKERS", tc.env)
		if got := queueWorkers(); got != tc.want {
			t.Errorf("%s: queueWorkers() = %d, want %d", name, got, tc.want)
		}
	}
}
