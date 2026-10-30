package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
)

type autonomyTightenedPayload struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TightenAutonomy is task 6.9's fourth named operation: moves a session's
// pinned autonomy level strictly tighter, reusing
// internal/permissions.Autonomy's own ratchet (Tighten) to validate the
// move — the SAME type internal/tools/pipeline.go pins per session at first
// touch, so this function's refusal of a widening target is not a second,
// possibly-divergent copy of that rule, just this package's own instance of
// the one ratchet type. A successful tighten durably appends
// EventAutonomyTightened and updates sessions.autonomy_level in the SAME
// transaction — never a bare column write with no justifying event
// (Principle II).
func (c *Control) TightenAutonomy(ctx context.Context, tenantID, sessionID uuid.UUID, target string) error {
	targetLevel, err := permissions.ParseAutonomyLevel(target)
	if err != nil {
		return fmt.Errorf("runctl: tighten autonomy: %w", err)
	}
	return c.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		sess, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		currentLevel, err := permissions.ParseAutonomyLevel(sess.AutonomyLevel)
		if err != nil {
			return fmt.Errorf("runctl: tighten autonomy: session %s has an unparseable autonomy level %q: %w", sessionID, sess.AutonomyLevel, err)
		}
		ratchet := permissions.Pin(currentLevel)
		if err := ratchet.Tighten(targetLevel); err != nil {
			return fmt.Errorf("runctl: %w", err)
		}

		if err := tx.SetAutonomyLevel(ctx, sessionID, target); err != nil {
			return err
		}
		_, err = tx.Append(ctx, tenantID, sessionID, eventlog.EventAutonomyTightened, nil, nil,
			autonomyTightenedPayload{From: sess.AutonomyLevel, To: target})
		return err
	})
}
