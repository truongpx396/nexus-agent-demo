// Package audit implements the hash-chained audit receipt (README.md §5,
// Phase 5, pattern 31): one receipt per durably appended event, chained per
// session over PLAINTEXT DIGESTS (never Payload itself, so a lawful
// crypto-shredding erasure never breaks verification, FR-081), signed by
// cmd/signerd's sign-only key custody (task 5.1), and periodically anchored
// outside the transactional writer path (anchor.go) so a scheduled verifier
// (verify.go) can catch a break or a sequence gap.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Receipt is one row of audit_receipts (migrations/0006_audit.sql).
type Receipt struct {
	ReceiptID     uuid.UUID
	ReceiptSeq    int64
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	Seq           int64
	EventID       uuid.UUID
	EventType     string
	PayloadDigest []byte
	PrevHash      []byte // nil for a session's first receipt
	Hash          []byte
	Signature     []byte
	SignerKeyID   string
	CreatedAt     time.Time
}

// Chain builds and persists receipts. Construct once, share across every
// call in the process — it holds no per-call state itself (unlike
// permissions.Chain, there's no circuit breaker or cache to reuse; a single
// long-lived value is just conventional here, not load-bearing).
type Chain struct {
	Signer Signer
}

func NewChain(signer Signer) *Chain {
	return &Chain{Signer: signer}
}

// genesisPrevHash is the fixed 32-byte value hashInput uses in place of a
// nil PrevHash for a session's first receipt — a conventional zero-value
// genesis rather than a variable-length encoding, which keeps hashInput's
// byte layout fixed-width and unambiguous.
var genesisPrevHash = make([]byte, sha256.Size)

// hashInput builds the exact bytes Append signs and Verify recomputes: a
// fixed-width, unambiguous encoding (no delimiter-based string
// concatenation that two different inputs could collide into) —
// prevHash(32) || tenantID(16) || sessionID(16) || seq(8, big-endian) ||
// eventID(16) || len(eventType)(4, big-endian) || eventType || payloadDigest(32).
func hashInput(prevHash []byte, tenantID, sessionID uuid.UUID, seq int64, eventID uuid.UUID, eventType string, payloadDigest []byte) []byte {
	if prevHash == nil {
		prevHash = genesisPrevHash
	}
	buf := make([]byte, 0, sha256.Size+16+16+8+16+4+len(eventType)+len(payloadDigest))
	buf = append(buf, prevHash...)
	buf = append(buf, tenantID[:]...)
	buf = append(buf, sessionID[:]...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(seq)) //nolint:gosec // seq is a Postgres bigint sequence value, never negative in practice
	buf = append(buf, eventID[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(eventType))) //nolint:gosec // event type names are short, fixed constants
	buf = append(buf, eventType...)
	buf = append(buf, payloadDigest...)
	sum := sha256.Sum256(buf)
	return sum[:]
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
func (c *Chain) Append(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, seq int64, eventID uuid.UUID, eventType string, payloadDigest []byte) (Receipt, error) {
	prev, err := headHash(ctx, tx, sessionID)
	if err != nil {
		return Receipt{}, err
	}
	hash := hashInput(prev, tenantID, sessionID, seq, eventID, eventType, payloadDigest)

	sig, keyID, err := c.Signer.Sign(ctx, hash)
	if err != nil {
		return Receipt{}, fmt.Errorf("audit: sign receipt for session %s seq %d: %w", sessionID, seq, err)
	}

	r := Receipt{
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
		return Receipt{}, fmt.Errorf("audit: insert receipt for session %s seq %d: %w", sessionID, seq, err)
	}
	return r, nil
}
