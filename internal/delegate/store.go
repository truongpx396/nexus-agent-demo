package delegate

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound mirrors internal/oversight.ErrNotFound's own convention: "it
// never existed" and "RLS made a cross-tenant row invisible" are
// indistinguishable on purpose.
var ErrNotFound = errors.New("delegate: not found")

const delegationSelectSQL = `
	SELECT delegation_id, tenant_id, parent_session_id, child_session_id,
	       parent_tool_use_event_id, fanout_id, agent_id, task, scope_grant,
	       return_schema, status, result, reason, created_at, resolved_at
	FROM delegations`

type row interface {
	Scan(dest ...any) error
}

func scanDelegation(r row) (Delegation, error) {
	var d Delegation
	var scopeGrant, returnSchema, result []byte
	var toolUseEventID, fanoutID *uuid.UUID
	var reason *string
	err := r.Scan(
		&d.DelegationID, &d.TenantID, &d.ParentSessionID, &d.ChildSessionID,
		&toolUseEventID, &fanoutID, &d.AgentID, &d.Task, &scopeGrant,
		&returnSchema, &d.Status, &result, &reason, &d.CreatedAt, &d.ResolvedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Delegation{}, fmt.Errorf("%w: delegation", ErrNotFound)
		}
		return Delegation{}, fmt.Errorf("delegate: scan delegation: %w", err)
	}
	if toolUseEventID != nil {
		d.ParentToolUseEventID = *toolUseEventID
	}
	d.FanoutID = fanoutID
	if len(scopeGrant) > 0 {
		if err := json.Unmarshal(scopeGrant, &d.ScopeGrant); err != nil {
			return Delegation{}, fmt.Errorf("delegate: unmarshal scope_grant: %w", err)
		}
	}
	if len(returnSchema) > 0 {
		d.ReturnSchema = returnSchema
	}
	if len(result) > 0 {
		d.Result = result
	}
	if reason != nil {
		d.Reason = *reason
	}
	return d, nil
}
