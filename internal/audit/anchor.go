package audit

import (
	"time"

	"github.com/google/uuid"
)

// Anchor is one row of audit_anchors (migrations/0006_audit.sql).
type Anchor struct {
	AnchorID       uuid.UUID
	TenantID       uuid.UUID
	FromReceiptSeq int64
	ToReceiptSeq   int64
	Hash           []byte
	Signature      []byte
	SignerKeyID    string
	CreatedAt      time.Time
}
