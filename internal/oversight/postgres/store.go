// Package postgres is the Postgres adapter for internal/oversight: the
// approvals and input_requests tables, each decision's audit event appended in
// the same transaction, and the session and key reads a resumed run is rebuilt
// from. The rules about what may be decided and when are oversight's own.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Store implements oversight.Store, oversight.Sessions and
// oversight.KeyOpener over Postgres. Every call runs in the named tenant's own
// scope (row-level security), so another tenant's approval is indistinguishable
// from one that does not exist.
type Store struct {
	st     *store.Store
	keys   *cryptopg.KeyStore
	sealer *sealed.Sealer
}

var (
	_ oversight.Store     = (*Store)(nil)
	_ oversight.Sessions  = (*Store)(nil)
	_ oversight.KeyOpener = (*Store)(nil)
)

// New returns a Store. keys seals each decision's event under the session's own
// key and opens keys for a resumed run; chain may be nil, in which case events
// are appended without a signed receipt (a minimal wiring for tests).
func New(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain) *Store {
	return &Store{st: st, keys: keys, sealer: sealed.NewSealer(keys, chain)}
}

func (s *Store) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx oversight.Tx) error) error {
	return s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, &txn{Appender: s.sealer.Bind(tx), tx: tx})
	})
}

func (s *Store) Load(ctx context.Context, tenantID, sessionID uuid.UUID) (oversight.SessionInfo, []eventlog.Event, error) {
	var (
		sess    eventlog.Session
		history []eventlog.Event
	)
	err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if sess, err = store.GetSession(ctx, tx, sessionID); err != nil {
			return err
		}
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return oversight.SessionInfo{}, nil, err
	}
	return oversight.SessionInfo{AutonomyLevel: sess.AutonomyLevel}, history, nil
}

func (s *Store) Unwrap(ctx context.Context, tenantID uuid.UUID, keyID string) (crypto.DEK, error) {
	var dek crypto.DEK
	err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = s.keys.Unwrap(ctx, tx, keyID)
		return uerr
	})
	return dek, err
}

// txn is one open pgx transaction presented as an oversight.Tx. The embedded
// Appender is bound to the same transaction.
type txn struct {
	eventlog.Appender
	tx pgx.Tx
}

var _ oversight.Tx = (*txn)(nil)

// --- approvals ---

const approvalSelectSQL = `
	SELECT approval_id, tenant_id, session_id, tool_use_event_id, tool_id, ask_kind,
	       canonical_digest, context_package, assignee, status, granted_input,
	       expires_at, decided_at, decided_by, reason, created_at
	FROM approvals`

// row is the subset of pgx.Row and pgx.Rows the scanners need — lets one scan
// function serve both a single read and a list.
type row interface {
	Scan(dest ...any) error
}

func scanApproval(r row) (oversight.Approval, error) {
	var ap oversight.Approval
	var contextJSON, grantedInput []byte
	var decidedBy, reason *string
	err := r.Scan(
		&ap.ApprovalID, &ap.TenantID, &ap.SessionID, &ap.ToolUseEventID, &ap.ToolID, &ap.AskKind,
		&ap.CanonicalDigest, &contextJSON, &ap.Assignee, &ap.Status, &grantedInput,
		&ap.ExpiresAt, &ap.DecidedAt, &decidedBy, &reason, &ap.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return oversight.Approval{}, fmt.Errorf("%w: approval", oversight.ErrNotFound)
		}
		return oversight.Approval{}, fmt.Errorf("oversight: scan approval: %w", err)
	}
	if len(contextJSON) > 0 {
		if err := json.Unmarshal(contextJSON, &ap.Context); err != nil {
			return oversight.Approval{}, fmt.Errorf("oversight: unmarshal context_package: %w", err)
		}
	}
	if len(grantedInput) > 0 {
		ap.GrantedInput = json.RawMessage(grantedInput)
	}
	if decidedBy != nil {
		ap.DecidedBy = *decidedBy
	}
	if reason != nil {
		ap.Reason = *reason
	}
	return ap, nil
}

func (t *txn) InsertApproval(ctx context.Context, ap *oversight.Approval) error {
	contextJSON, err := json.Marshal(ap.Context)
	if err != nil {
		return fmt.Errorf("oversight: marshal context_package: %w", err)
	}
	return t.tx.QueryRow(ctx, `
		INSERT INTO approvals (
			approval_id, tenant_id, session_id, tool_use_event_id, tool_id, ask_kind,
			canonical_digest, context_package, assignee, status, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at`,
		ap.ApprovalID, ap.TenantID, ap.SessionID, ap.ToolUseEventID, ap.ToolID, ap.AskKind,
		ap.CanonicalDigest, contextJSON, ap.Assignee, ap.Status, ap.ExpiresAt,
	).Scan(&ap.CreatedAt)
}

func (t *txn) GetApproval(ctx context.Context, approvalID uuid.UUID) (oversight.Approval, error) {
	return scanApproval(t.tx.QueryRow(ctx, approvalSelectSQL+` WHERE approval_id = $1`, approvalID))
}

func (t *txn) LockApproval(ctx context.Context, approvalID uuid.UUID) (oversight.Approval, error) {
	return scanApproval(t.tx.QueryRow(ctx, approvalSelectSQL+` WHERE approval_id = $1 FOR UPDATE`, approvalID))
}

func (t *txn) PendingApprovals(ctx context.Context) ([]oversight.Approval, error) {
	rows, err := t.tx.Query(ctx, approvalSelectSQL+` WHERE status = 'pending' ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("oversight: list pending approvals: %w", err)
	}
	defer rows.Close()
	var out []oversight.Approval
	for rows.Next() {
		ap, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

func (t *txn) LockPendingApproval(ctx context.Context, sessionID uuid.UUID) (oversight.Approval, bool, error) {
	ap, err := scanApproval(t.tx.QueryRow(ctx, approvalSelectSQL+` WHERE session_id = $1 AND status = 'pending' FOR UPDATE`, sessionID))
	if err != nil {
		if errors.Is(err, oversight.ErrNotFound) {
			return oversight.Approval{}, false, nil
		}
		return oversight.Approval{}, false, err
	}
	return ap, true, nil
}

func (t *txn) SetApprovalStatus(ctx context.Context, approvalID uuid.UUID, status oversight.ApprovalStatus, decidedBy, reason string) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE approvals SET status = $2, decided_at = now(), decided_by = $3, granted_input = $4, reason = $5
		WHERE approval_id = $1`,
		approvalID, status, nullIfEmpty(decidedBy), []byte(nil), nullIfEmpty(reason),
	)
	if err != nil {
		return fmt.Errorf("oversight: update approval %s status: %w", approvalID, err)
	}
	return nil
}

func (t *txn) SetApprovalModified(ctx context.Context, approvalID uuid.UUID, decidedBy string, grantedInput, newDigest []byte) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE approvals SET status = $2, decided_at = now(), decided_by = $3, granted_input = $4, canonical_digest = $5
		WHERE approval_id = $1`,
		approvalID, oversight.ApprovalGrantedModified, nullIfEmpty(decidedBy), grantedInput, newDigest,
	)
	if err != nil {
		return fmt.Errorf("oversight: update approval %s (modified): %w", approvalID, err)
	}
	return nil
}

func (t *txn) SessionOwner(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	sess, err := store.GetSession(ctx, t.tx, sessionID)
	if err != nil {
		return uuid.Nil, err
	}
	return sess.UserID, nil
}

// --- input requests ---

const inputRequestSelectSQL = `
	SELECT input_request_id, tenant_id, session_id, question, schema, on_expiry,
	       default_assumption, status, answer, used_default, expires_at, decided_at, reason, created_at
	FROM input_requests`

func scanInputRequest(r row) (oversight.InputRequest, error) {
	var ir oversight.InputRequest
	var schema, defaultAssumption, answer []byte
	var reason *string
	err := r.Scan(
		&ir.InputRequestID, &ir.TenantID, &ir.SessionID, &ir.Question, &schema, &ir.OnExpiry,
		&defaultAssumption, &ir.Status, &answer, &ir.UsedDefault, &ir.ExpiresAt, &ir.DecidedAt, &reason, &ir.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return oversight.InputRequest{}, fmt.Errorf("%w: input request", oversight.ErrNotFound)
		}
		return oversight.InputRequest{}, fmt.Errorf("oversight: scan input request: %w", err)
	}
	if len(schema) > 0 {
		ir.Schema = json.RawMessage(schema)
	}
	if len(defaultAssumption) > 0 {
		ir.DefaultAssumption = json.RawMessage(defaultAssumption)
	}
	if len(answer) > 0 {
		ir.Answer = json.RawMessage(answer)
	}
	if reason != nil {
		ir.Reason = *reason
	}
	return ir, nil
}

func (t *txn) InsertInputRequest(ctx context.Context, ir *oversight.InputRequest) error {
	return t.tx.QueryRow(ctx, `
		INSERT INTO input_requests (
			input_request_id, tenant_id, session_id, question, schema, on_expiry, default_assumption, status, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING created_at`,
		ir.InputRequestID, ir.TenantID, ir.SessionID, ir.Question, []byte(ir.Schema), ir.OnExpiry, []byte(ir.DefaultAssumption), ir.Status, ir.ExpiresAt,
	).Scan(&ir.CreatedAt)
}

func (t *txn) LockInputRequest(ctx context.Context, inputRequestID uuid.UUID) (oversight.InputRequest, error) {
	return scanInputRequest(t.tx.QueryRow(ctx, inputRequestSelectSQL+` WHERE input_request_id = $1 FOR UPDATE`, inputRequestID))
}

func (t *txn) LockPendingInputRequests(ctx context.Context, sessionID uuid.UUID) ([]oversight.InputRequest, error) {
	rows, err := t.tx.Query(ctx, inputRequestSelectSQL+` WHERE session_id = $1 AND status = 'pending' FOR UPDATE`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("oversight: find pending input requests for session %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []oversight.InputRequest
	for rows.Next() {
		ir, err := scanInputRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ir)
	}
	return out, rows.Err()
}

func (t *txn) SetInputAnswered(ctx context.Context, inputRequestID uuid.UUID, answer []byte, usedDefault bool) error {
	_, err := t.tx.Exec(ctx, `UPDATE input_requests SET status = 'answered', decided_at = now(), answer = $2, used_default = $3 WHERE input_request_id = $1`,
		inputRequestID, answer, usedDefault)
	if err != nil {
		return fmt.Errorf("oversight: update input request %s: %w", inputRequestID, err)
	}
	return nil
}

func (t *txn) SetInputStatus(ctx context.Context, inputRequestID uuid.UUID, status oversight.InputStatus, reason string) error {
	_, err := t.tx.Exec(ctx, `UPDATE input_requests SET status = $2, decided_at = now(), reason = $3 WHERE input_request_id = $1`,
		inputRequestID, status, nullIfEmpty(reason))
	if err != nil {
		return fmt.Errorf("oversight: update input request %s status: %w", inputRequestID, err)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
