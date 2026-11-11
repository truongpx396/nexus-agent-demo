package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/queue"
)

// writeQueueGauges adds the job queue's depth to /metrics: how many jobs wait, are
// being worked, are backing off or failed for good, how old the oldest is, and how
// many workers this process runs. Queued growing while the age climbs is what
// workers falling behind looks like, which no golden signal shows. A queue that
// cannot report its depth, or a read that fails, leaves the gauges out and logs;
// it never fails the scrape.
func writeQueueGauges(ctx context.Context, w http.ResponseWriter, depth queue.Stater) {
	if depth == nil {
		return
	}
	st, err := depth.Stats(ctx)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: /metrics: read queue depth")
		return
	}
	writeGaugeHeader(w, "nexus_queue_jobs", "Jobs on the queue by state: queued (waiting), leased (being worked), delayed (backing off) and dead (failed for good).")
	for _, s := range []struct {
		state string
		n     int64
	}{{"queued", st.Queued}, {"leased", st.Leased}, {"delayed", st.Delayed}, {"dead", st.Dead}} {
		fmt.Fprintf(w, "nexus_queue_jobs{state=%q} %d\n", s.state, s.n) //nolint:errcheck // best-effort write to a scrape response
	}
	writeGaugeHeader(w, "nexus_queue_oldest_job_age_seconds", "Age of the oldest job still on the queue, queued or leased. Zero when it is empty.")
	fmt.Fprintf(w, "nexus_queue_oldest_job_age_seconds %f\n", st.OldestAge.Seconds()) //nolint:errcheck // best-effort write to a scrape response
	writeGaugeHeader(w, "nexus_queue_workers", "Queue workers this process runs: its ceiling on concurrently driven runs.")
	fmt.Fprintf(w, "nexus_queue_workers %d\n", queueWorkers()) //nolint:errcheck // best-effort write to a scrape response
}
