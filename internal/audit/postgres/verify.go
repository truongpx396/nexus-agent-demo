package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
)

// Verify replays every receipt and anchor for tenantID, recomputing each
// hash and signature from the stored structural fields (never Payload —
// this works identically whether or not the tenant's DEK has since been
// crypto-shredded, which is exactly what task 5.5's erasure test asserts)
// and cross-checking receipts against events for a sequence gap.
func (c *Chain) Verify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (audit.Report, error) {
	report := audit.Report{TenantID: tenantID}

	pub, currentKeyID, err := c.Signer.PublicKey(ctx)
	if err != nil {
		return audit.Report{}, fmt.Errorf("audit: load verifying public key: %w", err)
	}

	if err := verifyGaps(ctx, tx, tenantID, &report); err != nil {
		return audit.Report{}, err
	}
	if err := verifyReceiptChain(ctx, tx, tenantID, pub, currentKeyID, &report); err != nil {
		return audit.Report{}, err
	}
	if err := verifyAnchors(ctx, tx, tenantID, pub, currentKeyID, &report); err != nil {
		return audit.Report{}, err
	}
	return report, nil
}

func verifyGaps(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, report *audit.Report) error {
	rows, err := tx.Query(ctx, `
		SELECT e.session_id, e.seq
		FROM events e
		LEFT JOIN audit_receipts r ON r.session_id = e.session_id AND r.seq = e.seq
		WHERE e.tenant_id = $1 AND r.receipt_id IS NULL
		ORDER BY e.session_id, e.seq`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("audit: query sequence gaps for tenant %s: %w", tenantID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var g audit.Gap
		if err := rows.Scan(&g.SessionID, &g.MissingSeq); err != nil {
			return fmt.Errorf("audit: scan gap row: %w", err)
		}
		report.Gaps = append(report.Gaps, g)
	}
	return rows.Err()
}

func verifyReceiptChain(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, pub ed25519.PublicKey, currentKeyID string, report *audit.Report) error {
	rows, err := tx.Query(ctx, `
		SELECT session_id, seq, event_id, event_type, payload_digest, prev_hash, hash, signature, signer_key_id
		FROM audit_receipts WHERE tenant_id = $1 ORDER BY session_id, seq ASC`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("audit: query receipts for tenant %s: %w", tenantID, err)
	}
	defer rows.Close()

	var curSession uuid.UUID
	var expectedPrev []byte
	first := true
	for rows.Next() {
		var sessionID, eventID uuid.UUID
		var seq int64
		var eventType, signerKeyID string
		var payloadDigest, prevHash, hash, sig []byte
		if err := rows.Scan(&sessionID, &seq, &eventID, &eventType, &payloadDigest, &prevHash, &hash, &sig, &signerKeyID); err != nil {
			return fmt.Errorf("audit: scan receipt row: %w", err)
		}
		report.ReceiptsChecked++

		if first || sessionID != curSession {
			curSession, expectedPrev, first = sessionID, nil, false
		}
		if !bytesEqualNilAware(prevHash, expectedPrev) {
			report.Breaks = append(report.Breaks, audit.Break{SessionID: sessionID, Seq: seq, Kind: "hash_mismatch", Detail: "stored prev_hash does not match the previous receipt's hash"})
		}

		want := audit.HashInput(prevHash, tenantID, sessionID, seq, eventID, eventType, payloadDigest)
		if !bytesEqualNilAware(want, hash) {
			report.Breaks = append(report.Breaks, audit.Break{SessionID: sessionID, Seq: seq, Kind: "hash_mismatch", Detail: "recomputed hash does not match the stored hash"})
		} else if signerKeyID != currentKeyID {
			report.Breaks = append(report.Breaks, audit.Break{SessionID: sessionID, Seq: seq, Kind: "unverifiable_signer_key", Detail: fmt.Sprintf("signed by %q, current trusted key is %q", signerKeyID, currentKeyID)})
		} else if !ed25519.Verify(pub, hash, sig) {
			report.Breaks = append(report.Breaks, audit.Break{SessionID: sessionID, Seq: seq, Kind: "signature_invalid", Detail: "signature does not verify against the trusted public key"})
		}

		expectedPrev = hash
	}
	return rows.Err()
}

type anchorRow struct {
	anchorID       uuid.UUID
	fromSeq, toSeq int64
	hash, sig      []byte
	signerKeyID    string
}

func verifyAnchors(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, pub ed25519.PublicKey, currentKeyID string, report *audit.Report) error {
	// Collected into a slice (and rows fully closed) BEFORE the loop below
	// issues any further query — pgx allows only one active result set per
	// connection/tx at a time, and recomputeAnchorHash below runs its own
	// tx.Query per anchor, which would otherwise nest inside this one and
	// leave the connection "busy."
	rows, err := tx.Query(ctx, `
		SELECT anchor_id, from_receipt_seq, to_receipt_seq, hash, signature, signer_key_id
		FROM audit_anchors WHERE tenant_id = $1 ORDER BY to_receipt_seq ASC`,
		tenantID,
	)
	if err != nil {
		return fmt.Errorf("audit: query anchors for tenant %s: %w", tenantID, err)
	}
	var anchors []anchorRow
	for rows.Next() {
		var a anchorRow
		if err := rows.Scan(&a.anchorID, &a.fromSeq, &a.toSeq, &a.hash, &a.sig, &a.signerKeyID); err != nil {
			rows.Close()
			return fmt.Errorf("audit: scan anchor row: %w", err)
		}
		anchors = append(anchors, a)
	}
	rerr := rows.Err()
	rows.Close()
	if rerr != nil {
		return fmt.Errorf("audit: iterate anchors for tenant %s: %w", tenantID, rerr)
	}

	var prevAnchorHash []byte
	for _, a := range anchors {
		report.AnchorsChecked++

		want, werr := recomputeAnchorHash(ctx, tx, tenantID, a.fromSeq, a.toSeq, prevAnchorHash)
		if werr != nil {
			return werr
		}
		if !bytesEqualNilAware(want, a.hash) {
			report.Breaks = append(report.Breaks, audit.Break{Seq: a.toSeq, Kind: "hash_mismatch", Detail: fmt.Sprintf("anchor %s: recomputed aggregate hash does not match the stored hash", a.anchorID)})
		} else if a.signerKeyID != currentKeyID {
			report.Breaks = append(report.Breaks, audit.Break{Seq: a.toSeq, Kind: "unverifiable_signer_key", Detail: fmt.Sprintf("anchor %s: signed by %q, current trusted key is %q", a.anchorID, a.signerKeyID, currentKeyID)})
		} else if !ed25519.Verify(pub, a.hash, a.sig) {
			report.Breaks = append(report.Breaks, audit.Break{Seq: a.toSeq, Kind: "signature_invalid", Detail: fmt.Sprintf("anchor %s: signature does not verify against the trusted public key", a.anchorID)})
		}

		prevAnchorHash = a.hash
	}
	return nil
}

func recomputeAnchorHash(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, fromSeq, toSeq int64, prevAnchorHash []byte) ([]byte, error) {
	rows, err := tx.Query(ctx,
		`SELECT hash FROM audit_receipts WHERE tenant_id = $1 AND receipt_seq > $2 AND receipt_seq <= $3 ORDER BY receipt_seq ASC`,
		tenantID, fromSeq, toSeq,
	)
	if err != nil {
		return nil, fmt.Errorf("audit: load receipts (%d,%d] for tenant %s: %w", fromSeq, toSeq, tenantID, err)
	}
	defer rows.Close()

	h := sha256.New()
	if prevAnchorHash != nil {
		h.Write(prevAnchorHash)
	} else {
		h.Write(audit.GenesisPrevHash)
	}
	for rows.Next() {
		var receiptHash []byte
		if err := rows.Scan(&receiptHash); err != nil {
			return nil, fmt.Errorf("audit: scan receipt hash: %w", err)
		}
		h.Write(receiptHash)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// bytesEqualNilAware treats nil and a zero-length slice as equal — a
// receipt's prev_hash column round-trips a Go nil as SQL NULL, which pgx
// scans back as a nil []byte, so a strict bytes.Equal(nil, []byte{}) split
// would report a false break on every session's very first receipt.
func bytesEqualNilAware(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
