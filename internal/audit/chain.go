// Package audit implements the hash-chained audit receipt (README.md §5,
// Phase 5, pattern 31): one receipt per durably appended event, chained per
// session over PLAINTEXT DIGESTS (never Payload itself, so a lawful
// crypto-shredding erasure never breaks verification, FR-081), signed by
// cmd/signerd's sign-only key custody (task 5.1), and periodically anchored
// outside the transactional writer path (anchor.go) so a scheduled verifier
// (verify.go) can catch a break or a sequence gap.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
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

// GenesisPrevHash is the fixed 32-byte value HashInput uses in place of a
// nil PrevHash for a session's first receipt — a conventional zero-value
// genesis rather than a variable-length encoding, which keeps HashInput's
// byte layout fixed-width and unambiguous.
var GenesisPrevHash = make([]byte, sha256.Size)

// HashInput builds the exact bytes Append signs and Verify recomputes: a
// fixed-width, unambiguous encoding (no delimiter-based string
// concatenation that two different inputs could collide into) —
// prevHash(32) || tenantID(16) || sessionID(16) || seq(8, big-endian) ||
// eventID(16) || len(eventType)(4, big-endian) || eventType || payloadDigest(32).
func HashInput(prevHash []byte, tenantID, sessionID uuid.UUID, seq int64, eventID uuid.UUID, eventType string, payloadDigest []byte) []byte {
	if prevHash == nil {
		prevHash = GenesisPrevHash
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
