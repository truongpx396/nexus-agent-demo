package plan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// loadRunState mirrors internal/oversight.Resumer.loadRunState and
// internal/delegate.Delegations.loadRunState field-for-field — the same
// out-of-band rehydration idiom, reimplemented here for the same reason
// each of those gives: no live RunState.Seal to reuse.
func (e *Executor) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, store.Session, error) {
	var sess store.Session
	var history []store.Event
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sess, err = store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	}); err != nil {
		return nil, store.Session{}, err
	}

	decrypt := e.decryptWith(tenantID, sessionID)
	transcript, err := kernel.Rehydrate(ctx, history, decrypt)
	if err != nil {
		return nil, store.Session{}, err
	}

	var keyID string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].KeyID != crypto.ErasureKeyID {
			keyID = history[i].KeyID
			break
		}
	}
	if keyID == "" {
		return nil, store.Session{}, fmt.Errorf("plan: session %s has no active key in its history", sessionID)
	}
	var dek crypto.DEK
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = e.Keys.Unwrap(ctx, tx, keyID)
		return uerr
	}); err != nil {
		return nil, store.Session{}, err
	}

	seal := func(plaintext []byte) (sealed, digest []byte, kid string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("plan: seal event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	return &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal, History: history, Transcript: transcript}, sess, nil
}

// lastContentText decrypts the session's own final EventContent — an
// agent step's own return value, the same selective-decrypt idiom
// store.ReplayFullProjection already uses for a terminal event's exact
// reason.
func (e *Executor) lastContentText(ctx context.Context, tenantID, sessionID uuid.UUID) (string, error) {
	var history []store.Event
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	}); err != nil {
		return "", err
	}

	var last *store.Event
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Type == store.EventContent {
			last = &history[i]
			break
		}
	}
	if last == nil {
		return "", nil
	}

	decrypt := e.decryptWith(tenantID, sessionID)
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
