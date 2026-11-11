package main

import "github.com/rs/zerolog/log"

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
