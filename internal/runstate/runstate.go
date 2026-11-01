// Package runstate rebuilds a kernel.RunState from a session's durable history.
// Every service that drives a run from outside a live kernel.Run call — a crash
// resume (runctl), a parent resumed by its child's return (delegate), a plan
// step (plan) — has no RunState.Seal closure already in hand, so each used to
// carry its own copy of the same sequence: read the session and its sealed
// history, decrypt through a key cache, rehydrate the transcript, find the key
// the session currently seals under, and build a Seal bound to it. This is the
// one copy.
//
// It is generic over the caller's own unit-of-work type, so each service passes
// its own Store without this package knowing it: anything whose Tx can read
// session rows and open keys will do.
package runstate

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Unit is what Load needs from a service's tenant-scoped unit of work.
type Unit interface {
	eventlog.SessionRows
	crypto.Keys
}

// Store is any service's Store whose Tx is a Unit.
type Store[T Unit] interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx T) error) error
}

// Loaded is a session rebuilt for a resume.
type Loaded struct {
	State   *kernel.RunState
	Session eventlog.Session
	History []eventlog.Event
	// Decrypt opens any event of this session; a caller that needs more than the
	// transcript (the Rule-of-Two taint state, a content event) reuses it.
	Decrypt kernel.DecryptFunc
}

// Decryptor returns a kernel.DecryptFunc bound to (tenantID, sessionID) that
// caches each key's unwrapped data key for its own lifetime.
func Decryptor[T Unit](st Store[T], tenantID, sessionID uuid.UUID) kernel.DecryptFunc {
	cache := map[string]crypto.DEK{}
	return func(ctx context.Context, e eventlog.Event) ([]byte, error) {
		if e.KeyID == crypto.ErasureKeyID {
			return e.Payload, nil // an erasure record is deliberately plaintext
		}
		dek, ok := cache[e.KeyID]
		if !ok {
			if err := st.InTenant(ctx, tenantID, func(ctx context.Context, tx T) error {
				var err error
				dek, err = tx.Unwrap(ctx, e.KeyID)
				return err
			}); err != nil {
				return nil, err
			}
			cache[e.KeyID] = dek
		}
		return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
	}
}

// Seal returns a kernel.SealFunc that seals under dek for (tenantID, sessionID).
func Seal(dek crypto.DEK, tenantID, sessionID uuid.UUID) kernel.SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("runstate: seal resumed event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}

// ActiveKeyID is the key a session's events are currently sealed under: its
// most recent event whose key is not the erasure marker.
func ActiveKeyID(history []eventlog.Event) (string, error) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].KeyID != crypto.ErasureKeyID {
			return history[i].KeyID, nil
		}
	}
	return "", fmt.Errorf("runstate: session has no active key in its history")
}

// Load reads sessionID and its history, rehydrates the transcript, and returns a
// RunState whose Seal is bound to the session's active key. It does not seed the
// Rule-of-Two taint state: that goes into whichever tool pipeline the caller
// holds, so the caller does it with Loaded.History and Loaded.Decrypt.
func Load[T Unit](ctx context.Context, st Store[T], tenantID, sessionID uuid.UUID) (Loaded, error) {
	var (
		sess    eventlog.Session
		history []eventlog.Event
	)
	if err := st.InTenant(ctx, tenantID, func(ctx context.Context, tx T) error {
		var err error
		if sess, err = tx.GetSession(ctx, sessionID); err != nil {
			return err
		}
		history, err = tx.ListEvents(ctx, sessionID)
		return err
	}); err != nil {
		return Loaded{}, err
	}

	decrypt := Decryptor(st, tenantID, sessionID)
	transcript, toolUseIDs, err := kernel.Rehydrate(ctx, history, decrypt)
	if err != nil {
		return Loaded{}, err
	}

	keyID, err := ActiveKeyID(history)
	if err != nil {
		return Loaded{}, err
	}
	var dek crypto.DEK
	if err := st.InTenant(ctx, tenantID, func(ctx context.Context, tx T) error {
		var err error
		dek, err = tx.Unwrap(ctx, keyID)
		return err
	}); err != nil {
		return Loaded{}, err
	}

	return Loaded{
		State:   &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: Seal(dek, tenantID, sessionID), History: history, Transcript: transcript, ToolUseIDs: toolUseIDs},
		Session: sess, History: history, Decrypt: decrypt,
	}, nil
}
