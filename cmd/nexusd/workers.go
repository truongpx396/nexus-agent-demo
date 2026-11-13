package main

import (
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// maxQueueWorkers bounds NEXUS_QUEUE_WORKERS: each worker holds a session lock, a
// provider stream and database connections while it drives a run, so a typo should
// not become a thousand of them.
const maxQueueWorkers = 64

// queueWorkers is how many queue workers this process runs, which is also its
// ceiling on concurrently driven runs. NEXUS_QUEUE_WORKERS sets it; anything
// outside 1 to maxQueueWorkers falls back to the default and says so.
func queueWorkers() int {
	n := envIntOr("NEXUS_QUEUE_WORKERS", queueWorkerCount)
	if n < 1 || n > maxQueueWorkers {
		log.Warn().Int("requested", n).Int("using", queueWorkerCount).Msg("nexusd: NEXUS_QUEUE_WORKERS out of range 1-64; using the default")
		return queueWorkerCount
	}
	return n
}

// defaultShutdownGrace is how long a stopping process lets the runs already under
// way finish before it cancels them. It is set against the orchestrator's own
// termination grace period, which has to cover the HTTP drain (30s, runHTTPServer)
// and this: the default of 25s fits a 60s allowance with room to spare, and a
// deployment whose runs are longer raises both.
const defaultShutdownGrace = 25 * time.Second

// shutdownGrace is NEXUS_SHUTDOWN_GRACE, a Go duration ("25s", "2m"). Anything that
// is not a positive duration falls back to the default and says so.
func shutdownGrace() time.Duration {
	raw := os.Getenv("NEXUS_SHUTDOWN_GRACE")
	if raw == "" {
		return defaultShutdownGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn().Str("requested", raw).Dur("using", defaultShutdownGrace).Msg("nexusd: NEXUS_SHUTDOWN_GRACE is not a positive duration; using the default")
		return defaultShutdownGrace
	}
	return d
}

// drainWorkers waits for the queue workers to return, which they do once the job
// each is running has finished. It gives them grace; if they are still going after
// that it calls cancelJobs, which ends the runs (the session lock is released and
// the job failed back onto the queue, so the next worker resumes it), and waits for
// the workers to unwind. It reports whether the grace period ran out.
func drainWorkers(workers *sync.WaitGroup, grace time.Duration, cancelJobs func()) (forced bool) {
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return false
	case <-timer.C:
		log.Warn().Dur("grace", grace).Msg("nexusd: runs still going after the shutdown grace period; cancelling them")
		cancelJobs()
		<-done
		return true
	}
}
