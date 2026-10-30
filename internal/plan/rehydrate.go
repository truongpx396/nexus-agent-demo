package plan

import (
	"context"
	"encoding/json"

	"github.com/truongpx396/nexus-agent-demo/internal/runstate"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// loadRunState rebuilds the session's RunState from its own durable history
// (runstate.Load): the plan executor drives kernel.Continue from outside any
// live kernel.Run call, with no RunState.Seal to reuse.
func (e *Executor) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, eventlog.Session, error) {
	loaded, err := runstate.Load[Tx](ctx, e.Store, tenantID, sessionID)
	if err != nil {
		return nil, eventlog.Session{}, err
	}
	return loaded.State, loaded.Session, nil
}

// lastContentText decrypts the session's own final EventContent — an
// agent step's own return value, the same selective-decrypt idiom
// eventlog.ReplayFullProjection already uses for a terminal event's exact
// reason.
func (e *Executor) lastContentText(ctx context.Context, tenantID, sessionID uuid.UUID) (string, error) {
	var history []eventlog.Event
	if err := e.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		history, err = tx.ListEvents(ctx, sessionID)
		return err
	}); err != nil {
		return "", err
	}

	var last *eventlog.Event
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Type == eventlog.EventContent {
			last = &history[i]
			break
		}
	}
	if last == nil {
		return "", nil
	}

	decrypt := runstate.Decryptor[Tx](e.Store, tenantID, sessionID)
	plaintext, err := decrypt(ctx, *last)
	if err != nil {
		return "", err
	}
	var payload struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return "", err
	}
	return payload.Body, nil
}
