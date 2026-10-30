package runctl

import (
	"context"

	"github.com/truongpx396/nexus-agent-demo/internal/runstate"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// loadRunState rebuilds a kernel.RunState from sessionID's own durable history
// (runstate.Load) and restores the session's Rule-of-Two taint state before
// anything else (Resume/ResumeConversation, both of which share this helper)
// can dispatch its next tool call — kernel.TaintSeeder's own doc comment on why
// this can't just happen lazily inside the tool executor itself.
func (c *Control) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, eventlog.Session, error) {
	loaded, err := runstate.Load[Tx](ctx, c.Store, tenantID, sessionID)
	if err != nil {
		return nil, eventlog.Session{}, err
	}
	engaged, err := kernel.RehydrateTaint(ctx, loaded.History, loaded.Decrypt)
	if err != nil {
		return nil, eventlog.Session{}, err
	}
	if seeder, ok := c.Kernel.Tools.(kernel.TaintSeeder); ok {
		seeder.SeedTaint(sessionID, loaded.Session.AutonomyLevel, engaged)
	}
	return loaded.State, loaded.Session, nil
}

// decryptFuncFor returns a kernel.DecryptFunc bound to (tenantID, sessionID),
// shared by Fork (for kernel.Rehydrate) and Replay (for verifying the upcast
// path against real plaintext payload shapes).
func (c *Control) decryptFuncFor(tenantID, sessionID uuid.UUID) kernel.DecryptFunc {
	return runstate.Decryptor[Tx](c.Store, tenantID, sessionID)
}
