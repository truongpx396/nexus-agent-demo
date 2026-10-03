package obs

import (
	"github.com/grafana/pyroscope-go"
	"github.com/rs/zerolog/log"
)

// PyroscopeConfig is the explicit, caller-supplied config StartPyroscope
// needs — same shape as newSpanExporter's own env-vars-read-by-the-caller
// convention (cmd/nexusd/provider.go): this package never reads an
// environment variable itself (InitLogger is this codebase's one
// intentional exception, documented on its own doc comment), so nexusd and
// signerd each read their own NEXUS_PYROSCOPE_* / NEXUS_PPROF_* vars and
// build this struct.
type PyroscopeConfig struct {
	// ServerAddress is the Pyroscope server's push endpoint, e.g.
	// http://localhost:4141 (make profiling-up, docs/observability.md).
	ServerAddress string
	// ApplicationName is Pyroscope's own top-level app selector — "nexusd"
	// or "signerd", the same two names docker-compose.yml's app profile and
	// promtail-config.yml's service label already use to tell the two
	// binaries apart in every other observability surface.
	ApplicationName string
	// Tags attaches static key/value labels to every profile this process
	// pushes (e.g. git_commit) — Pyroscope's own per-series labels, the same
	// idea as internal/obs span attributes but scoped to profiling, not
	// tracing.
	Tags map[string]string
	// MutexBlockProfiling adds the mutex/block profile types to what's
	// pushed. Only meaningful once the caller has ALSO called
	// EnableMutexBlockProfiling with a nonzero rate — turning this on
	// without that call still uploads a profile every UploadRate interval,
	// just an empty one, since the underlying runtime sampler was never
	// switched on.
	MutexBlockProfiling bool
}

// StartPyroscope begins continuous profiling to a Grafana Pyroscope server
// (docs/observability.md's "profiling" compose profile — a SEPARATE opt-in
// profile from "observability"/"tracing", same reasoning as Tempo's own:
// bringing up the base metrics/logs/alerts stack never pulls this in, and a
// demo that never runs `make profiling-up` pays nothing).
//
// Unlike Tempo/Langfuse's per-run trace spans, a Pyroscope profile is
// process-wide and content-free by construction — it's function names and
// call-stack sample counts, never a run's actual input/output, so it needs
// no allowlist filtering (internal/obs/allowlist.go) the way a span's
// attributes do (constitution Principle VI). It's also NOT tenant-scoped:
// one profile covers every tenant's work interleaved on this process, the
// same "process resource usage, not per-run behavior" pillar cAdvisor's own
// container metrics already occupy in docs/observability.md's architecture
// diagram — correlate it against a specific tenant's slow run via the
// timestamp overlap with that run's own trace (Tempo/Langfuse), not a
// shared label.
//
// cfg.ServerAddress == "" disables this and returns (nil, nil) — the same
// zero-setup default every optional exporter in this codebase follows
// (nothing set means nothing dialed). The returned *pyroscope.Profiler's
// own Stop() uploads whatever profiling data hasn't shipped yet before
// returning — callers should defer it exactly like they defer a span
// exporter's Shutdown.
func StartPyroscope(cfg PyroscopeConfig) (*pyroscope.Profiler, error) {
	if cfg.ServerAddress == "" {
		return nil, nil
	}

	profileTypes := append([]pyroscope.ProfileType{}, pyroscope.DefaultProfileTypes...)
	profileTypes = append(profileTypes, pyroscope.ProfileGoroutines)
	if cfg.MutexBlockProfiling {
		profileTypes = append(profileTypes,
			pyroscope.ProfileMutexCount, pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount, pyroscope.ProfileBlockDuration,
		)
	}

	return pyroscope.Start(pyroscope.Config{
		ApplicationName: cfg.ApplicationName,
		ServerAddress:   cfg.ServerAddress,
		Tags:            cfg.Tags,
		ProfileTypes:    profileTypes,
		Logger:          pyroscopeZerologAdapter{},
	})
}

// pyroscopeZerologAdapter routes the pyroscope-go SDK's own internal
// logging (upload retries/failures) through the SAME zerolog sink
// InitLogger already installed, rather than pyroscope.StandardLogger's
// stdlib log output — the latter would bypass NEXUS_LOG_FILE entirely, so
// Promtail (docs/observability.md) would never see an upload failure that
// every other warning/error in this process already surfaces through.
type pyroscopeZerologAdapter struct{}

func (pyroscopeZerologAdapter) Infof(format string, args ...interface{}) {
	log.Info().Msgf("pyroscope: "+format, args...)
}

func (pyroscopeZerologAdapter) Debugf(format string, args ...interface{}) {
	log.Debug().Msgf("pyroscope: "+format, args...)
}

func (pyroscopeZerologAdapter) Errorf(format string, args ...interface{}) {
	log.Error().Msgf("pyroscope: "+format, args...)
}
