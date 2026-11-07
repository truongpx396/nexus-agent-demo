package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
)

// openOrFindClaim is the write-ahead half of task 6.6: it durably records a
// FRESH in_flight claim for (sessionID, digest) if none exists yet (created
// = true), or returns the EXISTING row unchanged if one does (created =
// false) — "completed short-circuits" (a caller finding runctl.ClaimCompleted back
// must not call the tool again), and an existing runctl.ClaimInFlight is just as
// much a refusal to proceed (task 6.6: "never by re-execution") — it means
// either a genuine concurrent duplicate or a crash-orphaned prior attempt
// nobody has resolved yet (internal/runctl.ResolveClaim), and this function
// cannot tell those apart, so it treats both identically: hand the row
// back, change nothing, and let the caller (internal/tools/pipeline.go, via
// internal/runctl's tools.Claims implementation) refuse. The UNIQUE
// (session_id, canonical_digest) constraint (migrations/0012_claims.sql) is
// what makes the insert-or-fetch atomic under concurrent callers — INSERT
// ... ON CONFLICT DO NOTHING RETURNING, falling back to a SELECT only when
// the RETURNING clause found nothing (a real conflict), rather than a racy
// SELECT-then-INSERT.
func openOrFindClaim(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (claim runctl.Claim, created bool, err error) {
	claimID := uuid.New()
	row := tx.QueryRow(ctx, `
		INSERT INTO claims (claim_id, tenant_id, session_id, tool_id, canonical_digest, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (session_id, canonical_digest) DO NOTHING
		RETURNING claim_id, tenant_id, session_id, tool_id, canonical_digest, status, reason, created_at, resolved_at`,
		claimID, tenantID, sessionID, toolID, digest, runctl.ClaimInFlight,
	)
	var c runctl.Claim
	var reason *string
	scanErr := row.Scan(&c.ClaimID, &c.TenantID, &c.SessionID, &c.ToolID, &c.CanonicalDigest, &c.Status, &reason, &c.CreatedAt, &c.ResolvedAt)
	if scanErr == nil {
		if reason != nil {
			c.Reason = *reason
		}
		return c, true, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return runctl.Claim{}, false, fmt.Errorf("open claim: %w", scanErr)
	}

	// A real conflict: another row already exists for this
	// (session_id, canonical_digest) — read it back unchanged.
	c, err = scanClaim(tx.QueryRow(ctx, claimSelectSQL+` WHERE session_id = $1 AND canonical_digest = $2`, sessionID, digest))
	if err != nil {
		return runctl.Claim{}, false, fmt.Errorf("open claim: read back after conflict: %w", err)
	}
	return c, false, nil
}

// resolveClaim marks claimID completed or abandoned — the ONLY two ways a
// claim ever leaves runctl.ClaimInFlight (Complete, called by
// internal/tools/pipeline.go right after Tool.Call returns, and
// internal/runctl.ResolveClaim, called by a human or a probe for a claim a
// crash orphaned). Resolving an already-resolved claim is a no-op that
// returns the existing row rather than erroring — idempotent, matching
// every other "second call is harmless" convention in this codebase
// (oversight.Approvals.Invalidate's own doc comment on a no-pending-
// approval session).
func resolveClaim(ctx context.Context, tx pgx.Tx, claimID uuid.UUID, status runctl.ClaimStatus, reason string) (runctl.Claim, error) {
	if status != runctl.ClaimCompleted && status != runctl.ClaimAbandoned {
		return runctl.Claim{}, fmt.Errorf("resolve claim %s: status must be completed or abandoned, got %q", claimID, status)
	}
	_, err := tx.Exec(ctx, `
		UPDATE claims SET status = $2, reason = $3, resolved_at = now()
		WHERE claim_id = $1 AND status = 'in_flight'`,
		claimID, status, nullIfEmpty(reason),
	)
	if err != nil {
		return runctl.Claim{}, fmt.Errorf("resolve claim %s: %w", claimID, err)
	}
	c, err := scanClaim(tx.QueryRow(ctx, claimSelectSQL+` WHERE claim_id = $1`, claimID))
	if err != nil {
		return runctl.Claim{}, fmt.Errorf("resolve claim %s: read back: %w", claimID, err)
	}
	return c, nil
}

// listInFlightClaims returns every still-open claim for sessionID — what
// runctl.Resume consults before letting a crash-recovered run
// continue, and what a Checkpoint's OpenClaimID field is filled from at
// checkpoint time.
func listInFlightClaims(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) ([]runctl.Claim, error) {
	rows, err := tx.Query(ctx, claimSelectSQL+` WHERE session_id = $1 AND status = 'in_flight' ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list in-flight claims for session %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []runctl.Claim
	for rows.Next() {
		c, err := scanClaimRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const claimSelectSQL = `
	SELECT claim_id, tenant_id, session_id, tool_id, canonical_digest, status, reason, created_at, resolved_at
	FROM claims`

func scanClaim(r rowScanner) (runctl.Claim, error) { return scanClaimRows(r) }
func scanClaimRows(r rowScanner) (runctl.Claim, error) {
	var c runctl.Claim
	var reason *string
	err := r.Scan(&c.ClaimID, &c.TenantID, &c.SessionID, &c.ToolID, &c.CanonicalDigest, &c.Status, &reason, &c.CreatedAt, &c.ResolvedAt)
	if err != nil {
		return runctl.Claim{}, fmt.Errorf("scan claim: %w", err)
	}
	if reason != nil {
		c.Reason = *reason
	}
	return c, nil
}

// rowScanner is the subset of pgx.Row/pgx.Rows every scan helper in this
// package needs — mirrors internal/oversight's own `row` interface
// one-for-one, redeclared here rather than shared because the two packages
// have no other reason to depend on each other.
type rowScanner interface {
	Scan(dest ...any) error
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
