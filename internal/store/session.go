package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session is the subset of the sessions table (migration 0002) Phase 2
// reads and writes. Columns this phase never touches (fork_*, plan_*,
// delegation_role, priority, region, ...) keep their schema defaults —
// they're seams for the phases that own them, not dead weight to model here.
//
// Status and TerminalReason are marked PROJECTION in the schema comment, but
// Phase 2 writes them directly (UpdateSessionStatus, in the same transaction
// as the EventTerminal append that changes them) rather than deriving them
// by replaying the log on every read: the log is still the only source of
// truth (an event log replay from empty events would be undefined, not
// stale), but replay-on-read would need to decrypt every event's Payload
// just to answer "is this run done yet" — these two columns exist in the
// schema precisely so that question doesn't require a decrypt. Never written
// independently of the event that justifies the change is what keeps this
// from becoming a second source of truth.
type Session struct {
	SessionID      uuid.UUID
	SessionKey     string
	TenantID       uuid.UUID
	SurfaceID      string
	UserID         uuid.UUID
	AgentID        uuid.UUID
	AgentVersion   int
	HarnessDigest  []byte
	DataLabel      string
	RouteModelID   string
	RouteReason    map[string]string
	AutonomyLevel  string
	RootSessionID  uuid.UUID
	Depth          int
	DelegationRole string
	Status         string
	TerminalReason *string

	// ForkedFromSessionID/ForkSeq/ForkOverrides are the fork lineage columns
	// migrations/0002_sessions.sql seamed in at Phase 1, populated
	// meaningfully starting Phase 6 (README task 6.11,
	// internal/runctl.Fork). Nil/zero for every non-forked session — which
	// is every session before this phase.
	ForkedFromSessionID *uuid.UUID
	ForkSeq             *int64
	ForkOverrides       map[string]string
}

// CreateSession inserts a new session row. Must run inside a tenant-scoped
// transaction (store.Store.InTenantTx) like every other write in this
// package.
func CreateSession(ctx context.Context, tx pgx.Tx, s Session) error {
	reason, err := json.Marshal(s.RouteReason)
	if err != nil {
		return fmt.Errorf("marshal route_reason: %w", err)
	}
	overrides, err := json.Marshal(s.ForkOverrides)
	if err != nil {
		return fmt.Errorf("marshal fork_overrides: %w", err)
	}
	root := s.RootSessionID
	if root == uuid.Nil {
		root = s.SessionID // a fresh, non-delegated run is its own root (README §4 schema comment)
	}
	delegationRole := s.DelegationRole
	if delegationRole == "" {
		delegationRole = "root"
	}
	autonomy := s.AutonomyLevel
	if autonomy == "" {
		autonomy = "supervised"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO sessions (
			session_id, session_key, tenant_id, surface_id, user_id,
			agent_id, agent_version, harness_digest,
			data_label, route_model_id, route_reason,
			autonomy_level, root_session_id, depth, delegation_role,
			forked_from_session_id, fork_seq, fork_overrides
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		s.SessionID, s.SessionKey, s.TenantID, s.SurfaceID, s.UserID,
		s.AgentID, s.AgentVersion, s.HarnessDigest,
		s.DataLabel, s.RouteModelID, reason,
		autonomy, root, s.Depth, delegationRole,
		s.ForkedFromSessionID, s.ForkSeq, overrides,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession loads one session row, RLS-scoped like every read in this
// package.
func GetSession(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (Session, error) {
	var s Session
	var reason, overrides []byte
	err := tx.QueryRow(ctx, `
		SELECT session_id, session_key, tenant_id, surface_id, user_id,
		       agent_id, agent_version, harness_digest,
		       data_label, route_model_id, route_reason,
		       autonomy_level, root_session_id, depth, delegation_role,
		       status, terminal_reason,
		       forked_from_session_id, fork_seq, fork_overrides
		FROM sessions WHERE session_id = $1`, sessionID,
	).Scan(
		&s.SessionID, &s.SessionKey, &s.TenantID, &s.SurfaceID, &s.UserID,
		&s.AgentID, &s.AgentVersion, &s.HarnessDigest,
		&s.DataLabel, &s.RouteModelID, &reason,
		&s.AutonomyLevel, &s.RootSessionID, &s.Depth, &s.DelegationRole,
		&s.Status, &s.TerminalReason,
		&s.ForkedFromSessionID, &s.ForkSeq, &overrides,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Session{}, fmt.Errorf("session %s not found (wrong tenant scope, or it does not exist)", sessionID)
		}
		return Session{}, fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if len(reason) > 0 {
		if err := json.Unmarshal(reason, &s.RouteReason); err != nil {
			return Session{}, fmt.Errorf("unmarshal route_reason: %w", err)
		}
	}
	if len(overrides) > 0 {
		if err := json.Unmarshal(overrides, &s.ForkOverrides); err != nil {
			return Session{}, fmt.Errorf("unmarshal fork_overrides: %w", err)
		}
	}
	return s, nil
}

// ListEvents returns every event for sessionID in seq order — the full
// history Hygiene and promptctx.Build work from. RLS-scoped like every read
// in this package.
func ListEvents(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) ([]Event, error) {
	rows, err := tx.Query(ctx, `
		SELECT event_id, session_id, tenant_id, seq, schema_version, type,
		       payload, payload_digest, key_id, actor, tool_id, pair_ref,
		       model_id, trace_id, span_id, created_at
		FROM events WHERE session_id = $1 ORDER BY seq ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list events for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(
			&e.EventID, &e.SessionID, &e.TenantID, &e.Seq, &e.SchemaVersion, &e.Type,
			&e.Payload, &e.PayloadDigest, &e.KeyID, &e.Actor, &e.ToolID, &e.PairRef,
			&e.ModelID, &e.TraceID, &e.SpanID, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events for session %s: %w", sessionID, err)
	}
	return events, nil
}
