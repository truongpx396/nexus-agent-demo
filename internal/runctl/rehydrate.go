package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// loadRunState rebuilds a kernel.RunState from sessionID's own durable
// history — History, Transcript (via kernel.Rehydrate), and a Seal closure
// bound to the session's current active DEK. Near-identical to
// internal/oversight.Resumer's own unexported loadRunState (that package's
// doc comment explains why: this package operates out of band from any
// live kernel.Run call, with no RunState.Seal closure already in hand to
// reuse) — duplicated rather than shared for the same reason
// internal/runctl/append.go's activeKeyID/appendEvent already are.
func (c *Control) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, store.Session, error) {
	var sess store.Session
	var history []store.Event
	err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sess, err = store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return nil, store.Session{}, err
	}

	decrypt := c.decryptFuncFor(tenantID, sessionID)

	transcript, err := kernel.Rehydrate(ctx, history, decrypt)
	if err != nil {
		return nil, store.Session{}, err
	}

	keyID, err := currentActiveKeyID(history)
	if err != nil {
		return nil, store.Session{}, err
	}
	var dek crypto.DEK
	if err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = c.Keys.Unwrap(ctx, tx, keyID)
		return uerr
	}); err != nil {
		return nil, store.Session{}, err
	}

	seal := func(plaintext []byte) (sealed, digest []byte, kid string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("runctl: seal resumed event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	return &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal, History: history, Transcript: transcript}, sess, nil
}

// decryptFuncFor returns a kernel.DecryptFunc bound to (tenantID,
// sessionID), caching each key_id's unwrapped DEK for the life of the
// closure — shared by loadRunState (for kernel.Rehydrate) and Replay (for
// verifying the upcast path against real plaintext payload shapes).
func (c *Control) decryptFuncFor(tenantID, sessionID uuid.UUID) kernel.DecryptFunc {
	dekCache := map[string]crypto.DEK{}
	return func(ctx context.Context, e store.Event) ([]byte, error) {
		if e.KeyID == crypto.ErasureKeyID {
			return e.Payload, nil
		}
		if dek, ok := dekCache[e.KeyID]; ok {
			return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
		}
		var dek crypto.DEK
		if err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var uerr error
			dek, uerr = c.Keys.Unwrap(ctx, tx, e.KeyID)
			return uerr
		}); err != nil {
			return nil, err
		}
		dekCache[e.KeyID] = dek
		return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
	}
}

func currentActiveKeyID(history []store.Event) (string, error) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].KeyID != crypto.ErasureKeyID {
			return history[i].KeyID, nil
		}
	}
	return "", fmt.Errorf("runctl: session has no active key in its history")
}
