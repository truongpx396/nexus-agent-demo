package oversight

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
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

// NewInputs returns Inputs over store; like Approvals, every outcome runs in
// one unit of work with the event that records it.
func NewInputs(store Store) *Inputs {
	return &Inputs{deps: deps{Store: store}}
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
	err := i.Store.InTenant(ctx, p.TenantID, func(ctx context.Context, tx Tx) error {
		if err := tx.InsertInputRequest(ctx, &ir); err != nil {
			return err
		}
		_, err := tx.Append(ctx, p.TenantID, p.SessionID, eventlog.EventInputRequested, nil, nil, inputRequestedPayload{InputRequestID: ir.InputRequestID, Question: p.Question, Schema: p.Schema})
		return err
	})
	return ir, err
}

// Answer records a human's answer — carrying zero authorization value, per
// this type's own doc comment: it never satisfies a permission-chain Ask.
//
// A request past its TTL that nothing has swept yet is resolved first, per its
// own on_expiry policy (expireLocked), and that resolution is committed before
// the late answer is refused — refusing inside the unit would roll it back, and
// the recorded default assumption (or the typed expiry) would never happen.
func (i *Inputs) Answer(ctx context.Context, tenantID, inputRequestID uuid.UUID, answer json.RawMessage) (InputRequest, error) {
	var ir InputRequest
	var lapsed bool
	err := i.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ir, err = tx.LockInputRequest(ctx, inputRequestID)
		if err != nil {
			return err
		}
		if ir.Status == InputPending && time.Now().After(ir.ExpiresAt) {
			lapsed = true
			return i.expireLocked(ctx, tx, &ir)
		}
		if ir.Status != InputPending {
			return errInputNotPending(inputRequestID, ir.Status)
		}
		if err := tx.SetInputAnswered(ctx, inputRequestID, answer, false); err != nil {
			return err
		}
		ir.Status, ir.Answer = InputAnswered, answer
		_, err = tx.Append(ctx, tenantID, ir.SessionID, eventlog.EventInputAnswered, nil, nil, inputAnsweredPayload{InputRequestID: inputRequestID, Answer: answer})
		return err
	})
	if err != nil {
		return ir, err
	}
	if lapsed {
		return ir, errInputNotPending(inputRequestID, ir.Status)
	}
	return ir, nil
}

func errInputNotPending(inputRequestID uuid.UUID, status InputStatus) error {
	return fmt.Errorf("oversight: input request %s is %s, not pending", inputRequestID, status)
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
	err := i.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		loaded, err := tx.LockInputRequest(ctx, inputRequestID)
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

func (i *Inputs) expireLocked(ctx context.Context, tx Tx, ir *InputRequest) error {
	if ir.OnExpiry == OnExpiryDefault {
		if err := tx.SetInputAnswered(ctx, ir.InputRequestID, ir.DefaultAssumption, true); err != nil {
			return err
		}
		ir.Status, ir.Answer, ir.UsedDefault = InputAnswered, ir.DefaultAssumption, true
		_, err := tx.Append(ctx, ir.TenantID, ir.SessionID, eventlog.EventInputAnswered, nil, nil, inputAnsweredPayload{InputRequestID: ir.InputRequestID, Answer: ir.DefaultAssumption, UsedDefault: true})
		return err
	}
	if err := tx.SetInputStatus(ctx, ir.InputRequestID, InputExpired, ""); err != nil {
		return err
	}
	ir.Status = InputExpired
	_, err := tx.Append(ctx, ir.TenantID, ir.SessionID, eventlog.EventInputExpired, nil, nil, inputRequestIDPayload{InputRequestID: ir.InputRequestID})
	return err
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
