package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent/internal/queue"
)

type stubDepth struct {
	st  queue.Stats
	err error
}

func (s stubDepth) Stats(context.Context) (queue.Stats, error) { return s.st, s.err }

func TestWriteQueueGauges_ReportsEachStateTheAgeAndTheWorkers(t *testing.T) {
	t.Setenv("NEXUS_QUEUE_WORKERS", "5")
	rec := httptest.NewRecorder()
	writeQueueGauges(context.Background(), rec, stubDepth{st: queue.Stats{Queued: 7, Leased: 2, Delayed: 1, Dead: 3, OldestAge: 90 * time.Second}})

	body := rec.Body.String()
	for _, want := range []string{
		`nexus_queue_jobs{state="queued"} 7`, `nexus_queue_jobs{state="leased"} 2`,
		`nexus_queue_jobs{state="delayed"} 1`, `nexus_queue_jobs{state="dead"} 3`,
		"nexus_queue_oldest_job_age_seconds 90.000000", "nexus_queue_workers 5",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
}

// A queue that cannot say how deep it is, or a read that fails, must not take the
// rest of the scrape down with it.
func TestWriteQueueGauges_LeavesTheGaugesOutRatherThanFailTheScrape(t *testing.T) {
	for name, depth := range map[string]queue.Stater{
		"no stater": nil,
		"a failing": stubDepth{err: errors.New("redis down")},
	} {
		rec := httptest.NewRecorder()
		writeQueueGauges(context.Background(), rec, depth)
		if rec.Body.Len() != 0 {
			t.Errorf("%s: wrote %q, want nothing", name, rec.Body)
		}
	}
}
