// Package postgres is the Postgres adapter for the audit chain: it appends a
// hash-chained receipt in the caller's transaction (the same one that appended
// the event), anchors a tenant's receipts, and replays them to find a break or
// a gap. The receipt vocabulary, the hash, and the signer protocol are
// internal/audit, which has no storage.
//
// Every method takes the caller's pgx.Tx on purpose: Append runs in the SAME
// transaction as the event it receipts, so an event is never observable
// without its receipt.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
)

// Chain builds and persists receipts. Construct once, share across every
// call in the process — it holds no per-call state itself.
type Chain struct {
	Signer audit.Signer
}

func NewChain(signer audit.Signer) *Chain {
	return &Chain{Signer: signer}
}

// headHash loads the session's current chain head — the hash of its most
// recently appended receipt, or nil if this is the first. Safe to call
// without extra locking: every caller runs inside the same transaction as
// store.Append, which already holds pg_advisory_xact_lock(session_id) for
// the whole transaction (internal/store/append.go), so no concurrent writer
// can be mid-Append for this session at the same time.
func headHash(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) ([]byte, error) {
	var hash []byte
	err := tx.QueryRow(ctx,
		`SELECT hash FROM audit_receipts WHERE session_id = $1 ORDER BY seq DESC LIMIT 1`,
		sessionID,
	).Scan(&hash)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: load chain head for session %s: %w", sessionID, err)
	}
	return hash, nil
}

// Append extends tenantID/sessionID's chain by one receipt for the event
// identified by (seq, eventID, eventType, payloadDigest), inside the
// caller's transaction — the same one that appended the event itself
// (kernel.Kernel.Receipts, wired from cmd/nexusd), so an event is never
// observable without a receipt for it.
func (c *Chain) Append(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, seq int64, eventID uuid.UUID, eventType string, payloadDigest []byte) (audit.Receipt, error) {
	prev, err := headHash(ctx, tx, sessionID)
	if err != nil {
		return audit.Receipt{}, err
	}
	hash := audit.HashInput(prev, tenantID, sessionID, seq, eventID, eventType, payloadDigest)

	sig, keyID, err := c.Signer.Sign(ctx, hash)
	if err != nil {
		return audit.Receipt{}, fmt.Errorf("audit: sign receipt for session %s seq %d: %w", sessionID, seq, err)
	}

	r := audit.Receipt{
		ReceiptID: uuid.New(), TenantID: tenantID, SessionID: sessionID, Seq: seq,
		EventID: eventID, EventType: eventType, PayloadDigest: payloadDigest,
		PrevHash: prev, Hash: hash, Signature: sig, SignerKeyID: keyID,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO audit_receipts (
			receipt_id, tenant_id, session_id, seq, event_id, event_type,
			payload_digest, prev_hash, hash, signature, signer_key_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING receipt_seq, created_at`,
		r.ReceiptID, r.TenantID, r.SessionID, r.Seq, r.EventID, r.EventType,
		r.PayloadDigest, r.PrevHash, r.Hash, r.Signature, r.SignerKeyID,
	).Scan(&r.ReceiptSeq, &r.CreatedAt)
	if err != nil {
		return audit.Receipt{}, fmt.Errorf("audit: insert receipt for session %s seq %d: %w", sessionID, seq, err)
	}
	return r, nil
}
