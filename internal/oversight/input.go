package oversight

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Inputs implements the input-request lifecycle (README task 5.9):
// agent-to-human PULL, schema-declared question, resolving on expiry to
// either a recorded default assumption or a typed input_expired — and,
// unlike Approvals, carrying ZERO authorization value. An answer is made
// available to the model as an ordinary transcript fact (via the
// EventInputAnswered payload, which internal/promptctx's next turn folds in
// like any other event); it never satisfies a permission-chain Ask the way
// a grant does, and internal/tools has no "ExecuteWithInput" counterpart to
// ExecuteApproved — there is nothing for an answer to resume.
type Inputs struct {
	deps
	DefaultTTL time.Duration
}

func NewInputs(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *Inputs {
	return &Inputs{deps: deps{Store: st, Keys: keys, Chain: chain}}
}

func (i *Inputs) ttl() time.Duration {
	if i.DefaultTTL > 0 {
		return i.DefaultTTL
	}
	return DefaultApprovalTTL
}

// RequestInputParams is everything RequestInput needs.
type RequestInputParams struct {
	TenantID          uuid.UUID
	SessionID         uuid.UUID
	Question          string
	Schema            json.RawMessage
	OnExpiry          OnExpiry
	DefaultAssumption json.RawMessage
}

// RequestInput durably records a new input request and appends
// EventInputRequested. Unlike an approval (bound to an ALREADY-suspended
// run's pending tool_use), an input request carries no tool_use_event_id —
// it is the agent PULLING for information, not blocking on permission for
// an effect already attempted.
func (i *Inputs) RequestInput(ctx context.Context, p RequestInputParams) (InputRequest, error) {
	if p.OnExpiry == "" {
		p.OnExpiry = OnExpiryExpire
	}
	ir := InputRequest{
		InputRequestID: uuid.New(), TenantID: p.TenantID, SessionID: p.SessionID,
		Question: p.Question, Schema: p.Schema, OnExpiry: p.OnExpiry, DefaultAssumption: p.DefaultAssumption,
		Status: InputPending, ExpiresAt: time.Now().Add(i.ttl()),
	}
	err := i.Store.InTenantTx(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertInputRequest(ctx, tx, &ir); err != nil {
			return err
		}
		_, err := i.appendEvent(ctx, tx, p.TenantID, p.SessionID, store.EventInputRequested, nil, nil, inputRequestedPayload{InputRequestID: ir.InputRequestID, Question: p.Question, Schema: p.Schema})
		return err
	})
	return ir, err
}

// Answer records a human's answer — carrying zero authorization value, per
// this type's own doc comment: it never satisfies a permission-chain Ask.
func (i *Inputs) Answer(ctx context.Context, tenantID, inputRequestID uuid.UUID, answer json.RawMessage) (InputRequest, error) {
	var ir InputRequest
	err := i.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ir, err = i.checkPending(ctx, tx, inputRequestID)
		if err != nil {
			return err
		}
		if err := updateInputAnswered(ctx, tx, inputRequestID, answer, false); err != nil {
			return err
		}
		ir.Status, ir.Answer = InputAnswered, answer
		_, err = i.appendEvent(ctx, tx, tenantID, ir.SessionID, store.EventInputAnswered, nil, nil, inputAnsweredPayload{InputRequestID: inputRequestID, Answer: answer})
		return err
	})
	return ir, err
}

// Expire resolves one overdue pending request per its own declared
// on_expiry policy (task 5.9): OnExpiryDefault answers it with
// DefaultAssumption (EventInputAnswered, UsedDefault=true — the model sees
// an ordinary answer, just one nobody actually typed); OnExpiryExpire
// produces a typed EventInputExpired instead. Called lazily (checkPending,
// below) or by a periodic sweep — either way, idempotent: a request no
// longer pending is left alone.
func (i *Inputs) Expire(ctx context.Context, tenantID, inputRequestID uuid.UUID) (InputRequest, error) {
	var ir InputRequest
	err := i.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		loaded, err := scanInputRequest(tx.QueryRow(ctx, inputRequestSelectSQL+` WHERE input_request_id = $1 FOR UPDATE`, inputRequestID))
		if err != nil {
			return err
		}
		ir = loaded
		if ir.Status != InputPending {
			return nil // already resolved — idempotent no-op
		}
		return i.expireLocked(ctx, tx, &ir)
	})
	return ir, err
}

func (i *Inputs) expireLocked(ctx context.Context, tx pgx.Tx, ir *InputRequest) error {
	if ir.OnExpiry == OnExpiryDefault {
		if err := updateInputAnswered(ctx, tx, ir.InputRequestID, ir.DefaultAssumption, true); err != nil {
			return err
		}
		ir.Status, ir.Answer, ir.UsedDefault = InputAnswered, ir.DefaultAssumption, true
		_, err := i.appendEvent(ctx, tx, ir.TenantID, ir.SessionID, store.EventInputAnswered, nil, nil, inputAnsweredPayload{InputRequestID: ir.InputRequestID, Answer: ir.DefaultAssumption, UsedDefault: true})
		return err
	}
	if err := updateInputStatus(ctx, tx, ir.InputRequestID, InputExpired, ""); err != nil {
		return err
	}
	ir.Status = InputExpired
	_, err := i.appendEvent(ctx, tx, ir.TenantID, ir.SessionID, store.EventInputExpired, nil, nil, inputRequestIDPayload{InputRequestID: ir.InputRequestID})
	return err
}

// checkPending loads ir and lazily expires it (via expireLocked) if its TTL
// has passed but nothing swept it yet, returning an error only when it's
// left in a non-pending state Answer cannot act on.
func (i *Inputs) checkPending(ctx context.Context, tx pgx.Tx, inputRequestID uuid.UUID) (InputRequest, error) {
	ir, err := scanInputRequest(tx.QueryRow(ctx, inputRequestSelectSQL+` WHERE input_request_id = $1 FOR UPDATE`, inputRequestID))
	if err != nil {
		return InputRequest{}, err
	}
	if ir.Status == InputPending && time.Now().After(ir.ExpiresAt) {
		if err := i.expireLocked(ctx, tx, &ir); err != nil {
			return InputRequest{}, err
		}
	}
	if ir.Status != InputPending {
		return InputRequest{}, fmt.Errorf("oversight: input request %s is %s, not pending", inputRequestID, ir.Status)
	}
	return ir, nil
}

type inputRequestedPayload struct {
	InputRequestID uuid.UUID       `json:"input_request_id"`
	Question       string          `json:"question"`
	Schema         json.RawMessage `json:"schema,omitempty"`
}

type inputAnsweredPayload struct {
	InputRequestID uuid.UUID       `json:"input_request_id"`
	Answer         json.RawMessage `json:"answer"`
	UsedDefault    bool            `json:"used_default,omitempty"`
}

type inputRequestIDPayload struct {
	InputRequestID uuid.UUID `json:"input_request_id"`
}

const inputRequestSelectSQL = `
	SELECT input_request_id, tenant_id, session_id, question, schema, on_expiry,
	       default_assumption, status, answer, used_default, expires_at, decided_at, reason, created_at
	FROM input_requests`

func scanInputRequest(r row) (InputRequest, error) {
	var ir InputRequest
	var schema, defaultAssumption, answer []byte
	var reason *string
	err := r.Scan(
		&ir.InputRequestID, &ir.TenantID, &ir.SessionID, &ir.Question, &schema, &ir.OnExpiry,
		&defaultAssumption, &ir.Status, &answer, &ir.UsedDefault, &ir.ExpiresAt, &ir.DecidedAt, &reason, &ir.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return InputRequest{}, fmt.Errorf("%w: input request", ErrNotFound)
		}
		return InputRequest{}, fmt.Errorf("oversight: scan input request: %w", err)
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

func insertInputRequest(ctx context.Context, tx pgx.Tx, ir *InputRequest) error {
	return tx.QueryRow(ctx, `
		INSERT INTO input_requests (
			input_request_id, tenant_id, session_id, question, schema, on_expiry, default_assumption, status, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING created_at`,
		ir.InputRequestID, ir.TenantID, ir.SessionID, ir.Question, []byte(ir.Schema), ir.OnExpiry, []byte(ir.DefaultAssumption), ir.Status, ir.ExpiresAt,
	).Scan(&ir.CreatedAt)
}

func updateInputAnswered(ctx context.Context, tx pgx.Tx, inputRequestID uuid.UUID, answer []byte, usedDefault bool) error {
	_, err := tx.Exec(ctx, `UPDATE input_requests SET status = 'answered', decided_at = now(), answer = $2, used_default = $3 WHERE input_request_id = $1`,
		inputRequestID, answer, usedDefault)
	if err != nil {
		return fmt.Errorf("oversight: update input request %s: %w", inputRequestID, err)
	}
	return nil
}

func updateInputStatus(ctx context.Context, tx pgx.Tx, inputRequestID uuid.UUID, status InputStatus, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE input_requests SET status = $2, decided_at = now(), reason = $3 WHERE input_request_id = $1`,
		inputRequestID, status, nullIfEmpty(reason))
	if err != nil {
		return fmt.Errorf("oversight: update input request %s status: %w", inputRequestID, err)
	}
	return nil
}
