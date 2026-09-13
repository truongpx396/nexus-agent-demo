package obs

import (
	"io"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// InitLogger installs the global logger every package in this codebase
// already calls through (github.com/rs/zerolog/log.Logger) with the two
// things its zero-value default never had: an explicit level floor, and,
// optionally, a second JSON sink on disk — the append-only file a
// log-aggregation agent (Promtail, docs/observability.md) tails. Without it
// the default is exactly what it always was: unleveled JSON on stderr.
//
// nexusd and signerd both run as plain host processes under `make run`
// (docs/local-llm.md's own "Ollama runs natively" reasoning extends to the
// binaries themselves — no compose service wraps them by default), so
// Docker's own json-file log driver never sees their output the way it
// would for a containerized service. Teeing to a file under .dev/ (already
// gitignored — see .gitignore's own /.dev/ entry) is what lets Promtail
// reach these logs without requiring the app profile
// (deploy/docker-compose.yml --profile app) just to observe them.
//
// service names the process (deploy/observability/promtail-config.yml
// promotes it to a Loki label, "nexusd" or "signerd" — the same two names
// docker-compose.yml's own app profile already uses) so a Grafana query can
// tell the two apart even when both happen to log to the same aggregated
// view.
//
// NEXUS_LOG_LEVEL (default "info") takes any zerolog level name. An
// unparseable value falls back to info rather than failing closed — logging
// configuration is an observability concern, not a security one, so this is
// the one env knob in this codebase that does NOT follow the fail-closed
// convention internal/config's own validated Config applies to everything
// else (README task 14.4).
func InitLogger(service string) {
	level, err := zerolog.ParseLevel(envOr("NEXUS_LOG_LEVEL", "info"))
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	var w io.Writer = os.Stderr
	if path := envOr("NEXUS_LOG_FILE", ""); path != "" {
		// filepath.Clean, not the raw operator-supplied value: this is an
		// operator/deploy-time config knob (the same trust level as
		// NEXUS_KEK_PATH or NEXUS_SIGNER_KEY_PATH elsewhere in this
		// codebase), never request input, but the log destination is still
		// worth writing defensively rather than trusting verbatim.
		f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			// stderr-only is still a fully functional logger — this is a
			// degrade, not a failure, so it's reported through the
			// not-yet-reconfigured default logger rather than aborting.
			log.Warn().Err(err).Str("path", path).Msg("obs: could not open NEXUS_LOG_FILE, logging to stderr only")
		} else {
			w = io.MultiWriter(os.Stderr, f)
		}
	}
	log.Logger = zerolog.New(w).With().Timestamp().Str("service", service).Logger()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
