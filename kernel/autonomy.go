package kernel

import (
	"context"

	"github.com/rs/zerolog/log"
)

// currentAutonomy is the autonomy level the next tool calls run under: what the
// session is pinned at now, when the log can say, else what the run was
// configured with. A failed read falls back to the configured level and logs;
// it never fails the turn, because the pipeline keeps its own ratchet and will
// not loosen a level it has already seen tightened.
func (k *Kernel) currentAutonomy(ctx context.Context, st *RunState, cfg RunConfig) string {
	r, ok := k.Log.(AutonomyReader)
	if !ok {
		return cfg.AutonomyLevel
	}
	level, err := r.CurrentAutonomy(ctx, st.TenantID, st.SessionID)
	if err != nil {
		log.Warn().Err(err).Any("session_id", st.SessionID).Msg("kernel: could not read the session's current autonomy level; using the configured one")
		return cfg.AutonomyLevel
	}
	if level == "" {
		return cfg.AutonomyLevel
	}
	return level
}
