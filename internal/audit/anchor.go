package audit

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// Anchor computes and persists one new anchor for tenantID, covering every
// receipt written since the last anchor (task 5.3: "periodic head anchoring
// outside the writing system"). It runs as its own pass — a ticker in
// cmd/nexusd, or the `nexusd verify-chain` subcommand — deliberately NOT
// inside the same transaction as any single event append: an anchor's whole
// point is to be an independent checkpoint a later verify pass can compare
// the live receipt rows against, catching a retroactive edit that stayed
// internally consistent with the per-receipt chain alone.
//
// Returns created=false with a zero Anchor if there is nothing new to
// anchor (no receipts written since the last anchor) — this is the normal,
// frequent case for a ticker firing on an idle tenant, not an error.
func (c *Chain) Anchor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (a Anchor, created bool, err error) {
	var fromSeq int64
	var prevHash []byte
	row := tx.QueryRow(ctx, `SELECT to_receipt_seq, hash FROM audit_anchors WHERE tenant_id = $1 ORDER BY to_receipt_seq DESC LIMIT 1`, tenantID)
	if serr := row.Scan(&fromSeq, &prevHash); serr != nil {
		if serr != pgx.ErrNoRows {
			return Anchor{}, false, fmt.Errorf("audit: load last anchor for tenant %s: %w", tenantID, serr)
		}
		fromSeq, prevHash = 0, nil
	}

	var maxSeq int64
	if serr := tx.QueryRow(ctx, `SELECT COALESCE(MAX(receipt_seq), 0) FROM audit_receipts WHERE tenant_id = $1`, tenantID).Scan(&maxSeq); serr != nil {
		return Anchor{}, false, fmt.Errorf("audit: load max receipt_seq for tenant %s: %w", tenantID, serr)
	}
	if maxSeq <= fromSeq {
		return Anchor{}, false, nil
	}

	rows, qerr := tx.Query(ctx,
		`SELECT hash FROM audit_receipts WHERE tenant_id = $1 AND receipt_seq > $2 AND receipt_seq <= $3 ORDER BY receipt_seq ASC`,
		tenantID, fromSeq, maxSeq,
	)
	if qerr != nil {
		return Anchor{}, false, fmt.Errorf("audit: load receipts (%d,%d] for tenant %s: %w", fromSeq, maxSeq, tenantID, qerr)
	}
	defer rows.Close()

	h := sha256.New()
	if prevHash != nil {
		h.Write(prevHash)
	} else {
		h.Write(genesisPrevHash)
	}
	for rows.Next() {
		var receiptHash []byte
		if serr := rows.Scan(&receiptHash); serr != nil {
			return Anchor{}, false, fmt.Errorf("audit: scan receipt hash: %w", serr)
		}
		h.Write(receiptHash)
	}
	if rerr := rows.Err(); rerr != nil {
		return Anchor{}, false, fmt.Errorf("audit: iterate receipts for tenant %s: %w", tenantID, rerr)
	}
	aggregate := h.Sum(nil)

	sig, keyID, serr := c.Signer.Sign(ctx, aggregate)
	if serr != nil {
		return Anchor{}, false, fmt.Errorf("audit: sign anchor for tenant %s: %w", tenantID, serr)
	}

	a = Anchor{
		AnchorID: uuid.New(), TenantID: tenantID,
		FromReceiptSeq: fromSeq, ToReceiptSeq: maxSeq,
		Hash: aggregate, Signature: sig, SignerKeyID: keyID,
	}
	if ierr := tx.QueryRow(ctx, `
		INSERT INTO audit_anchors (anchor_id, tenant_id, from_receipt_seq, to_receipt_seq, hash, signature, signer_key_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING created_at`,
		a.AnchorID, a.TenantID, a.FromReceiptSeq, a.ToReceiptSeq, a.Hash, a.Signature, a.SignerKeyID,
	).Scan(&a.CreatedAt); ierr != nil {
		return Anchor{}, false, fmt.Errorf("audit: insert anchor for tenant %s: %w", tenantID, ierr)
	}
	return a, true, nil
}
