package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// CreateSession inserts a new session row. Must run inside a tenant-scoped
// transaction (store.Store.InTenantTx) like every other write in this
// package.
func CreateSession(ctx context.Context, tx pgx.Tx, s eventlog.Session) error {
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
			forked_from_session_id, fork_seq, fork_overrides,
			plan_id, plan_version, team_id, conversational
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		s.SessionID, s.SessionKey, s.TenantID, s.SurfaceID, s.UserID,
		s.AgentID, s.AgentVersion, s.HarnessDigest,
		s.DataLabel, s.RouteModelID, reason,
		autonomy, root, s.Depth, delegationRole,
		s.ForkedFromSessionID, s.ForkSeq, overrides,
		s.PlanID, s.PlanVersion, s.TeamID, s.Conversational,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// ErrSessionNotFound is what GetSession wraps when no row is visible: the
// session does not exist, or it belongs to another tenant (RLS cannot tell
// the two apart, and neither should the caller).
var ErrSessionNotFound = errors.New("session not found")

// GetSession loads one session row, RLS-scoped like every read in this
// package.
func GetSession(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (eventlog.Session, error) {
	var s eventlog.Session
	var reason, overrides []byte
	err := tx.QueryRow(ctx, `
		SELECT session_id, session_key, tenant_id, surface_id, user_id,
		       agent_id, agent_version, harness_digest,
		       data_label, route_model_id, route_reason,
		       autonomy_level, root_session_id, depth, delegation_role,
		       status, terminal_reason,
		       forked_from_session_id, fork_seq, fork_overrides,
		       plan_id, plan_version, team_id, created_at, conversational, updated_at
		FROM sessions WHERE session_id = $1`, sessionID,
	).Scan(
		&s.SessionID, &s.SessionKey, &s.TenantID, &s.SurfaceID, &s.UserID,
		&s.AgentID, &s.AgentVersion, &s.HarnessDigest,
		&s.DataLabel, &s.RouteModelID, &reason,
		&s.AutonomyLevel, &s.RootSessionID, &s.Depth, &s.DelegationRole,
		&s.Status, &s.TerminalReason,
		&s.ForkedFromSessionID, &s.ForkSeq, &overrides,
		&s.PlanID, &s.PlanVersion, &s.TeamID, &s.CreatedAt, &s.Conversational, &s.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return eventlog.Session{}, fmt.Errorf("session %s (wrong tenant scope, or it does not exist): %w", sessionID, ErrSessionNotFound)
		}
		return eventlog.Session{}, fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if len(reason) > 0 {
		if err := json.Unmarshal(reason, &s.RouteReason); err != nil {
			return eventlog.Session{}, fmt.Errorf("unmarshal route_reason: %w", err)
		}
	}
	if len(overrides) > 0 {
		if err := json.Unmarshal(overrides, &s.ForkOverrides); err != nil {
			return eventlog.Session{}, fmt.Errorf("unmarshal fork_overrides: %w", err)
		}
	}
	return s, nil
}

// GetSessionByKey loads the MOST RECENT session for (tenantID, sessionKey)
// — sessions_tenant_session_key_idx (migrations/0002_sessions.sql) backs
// this, but session_key carries no uniqueness constraint, deliberately:
// internal/surfaces/telegram (and zalo, email) reuse one deterministic key
// per peer (e.g. "telegram:{chat_id}") across every session that peer ever
// has, so an idle-timed-out or cancelled conversation's key gets reused by
// whatever session comes next for the same peer — only the latest one is
// ever a caller's concern. ok=false (not an error) is the ordinary "no
// session for this peer yet" case, mirroring ChannelPort.WebhookSecret's
// own (string, bool, error) convention in those same packages.
func GetSessionByKey(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, sessionKey string) (eventlog.Session, bool, error) {
	var s eventlog.Session
	var reason, overrides []byte
	err := tx.QueryRow(ctx, `
		SELECT session_id, session_key, tenant_id, surface_id, user_id,
		       agent_id, agent_version, harness_digest,
		       data_label, route_model_id, route_reason,
		       autonomy_level, root_session_id, depth, delegation_role,
		       status, terminal_reason,
		       forked_from_session_id, fork_seq, fork_overrides,
		       plan_id, plan_version, team_id, created_at, conversational, updated_at
		FROM sessions WHERE tenant_id = $1 AND session_key = $2
		ORDER BY created_at DESC LIMIT 1`, tenantID, sessionKey,
	).Scan(
		&s.SessionID, &s.SessionKey, &s.TenantID, &s.SurfaceID, &s.UserID,
		&s.AgentID, &s.AgentVersion, &s.HarnessDigest,
		&s.DataLabel, &s.RouteModelID, &reason,
		&s.AutonomyLevel, &s.RootSessionID, &s.Depth, &s.DelegationRole,
		&s.Status, &s.TerminalReason,
		&s.ForkedFromSessionID, &s.ForkSeq, &overrides,
		&s.PlanID, &s.PlanVersion, &s.TeamID, &s.CreatedAt, &s.Conversational, &s.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return eventlog.Session{}, false, nil
		}
		return eventlog.Session{}, false, fmt.Errorf("get session by key %q: %w", sessionKey, err)
	}
	if len(reason) > 0 {
		if err := json.Unmarshal(reason, &s.RouteReason); err != nil {
			return eventlog.Session{}, false, fmt.Errorf("unmarshal route_reason: %w", err)
		}
	}
	if len(overrides) > 0 {
		if err := json.Unmarshal(overrides, &s.ForkOverrides); err != nil {
			return eventlog.Session{}, false, fmt.Errorf("unmarshal fork_overrides: %w", err)
		}
	}
	return s, true, nil
}

// ListSessionsForUser returns userID's own root sessions (delegation_role =
// 'root' -- a delegated/team-member session isn't its own top-level thread;
// it's reached through its parent's own event log via child_session_id),
// newest first. RLS-scoped like every read in this package. Used by the web
// UI's session sidebar (internal/surfaces/rest's GET /v1/sessions) -- there
// is otherwise no "list runs" capability, only GetSession's lookup-by-id.
func ListSessionsForUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID, limit int) ([]eventlog.Session, error) {
	rows, err := tx.Query(ctx, `
		SELECT session_id, session_key, tenant_id, surface_id, user_id,
		       agent_id, agent_version, harness_digest,
		       data_label, route_model_id, route_reason,
		       autonomy_level, root_session_id, depth, delegation_role,
		       status, terminal_reason,
		       forked_from_session_id, fork_seq, fork_overrides,
		       plan_id, plan_version, team_id, created_at, conversational, updated_at
		FROM sessions
		WHERE user_id = $1 AND delegation_role = 'root'
		ORDER BY created_at DESC
		LIMIT $2`, userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions for user %s: %w", userID, err)
	}
	defer rows.Close()

	var out []eventlog.Session
	for rows.Next() {
		var s eventlog.Session
		var reason, overrides []byte
		if err := rows.Scan(
			&s.SessionID, &s.SessionKey, &s.TenantID, &s.SurfaceID, &s.UserID,
			&s.AgentID, &s.AgentVersion, &s.HarnessDigest,
			&s.DataLabel, &s.RouteModelID, &reason,
			&s.AutonomyLevel, &s.RootSessionID, &s.Depth, &s.DelegationRole,
			&s.Status, &s.TerminalReason,
			&s.ForkedFromSessionID, &s.ForkSeq, &overrides,
			&s.PlanID, &s.PlanVersion, &s.TeamID, &s.CreatedAt, &s.Conversational, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if len(reason) > 0 {
			if err := json.Unmarshal(reason, &s.RouteReason); err != nil {
				return nil, fmt.Errorf("unmarshal route_reason: %w", err)
			}
		}
		if len(overrides) > 0 {
			if err := json.Unmarshal(overrides, &s.ForkOverrides); err != nil {
				return nil, fmt.Errorf("unmarshal fork_overrides: %w", err)
			}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions for user %s: %w", userID, err)
	}
	return out, nil
}

// ListStaleAwaitingInputSessionIDs returns every session_id in
// SessionStatusAwaitingInput whose updated_at is older than olderThan --
// the idle-conversation sweep's own query (cmd/nexusd/background.go,
// mirroring internal/teams's own listStaleActiveTeamIDs wall-clock-backstop
// shape exactly). RLS-scoped like every read in this package: called once
// per tenant, inside that tenant's own InTenantTx.
func ListStaleAwaitingInputSessionIDs(ctx context.Context, tx pgx.Tx, olderThan time.Time) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT session_id FROM sessions WHERE status = $1 AND updated_at < $2`,
		eventlog.SessionStatusAwaitingInput, olderThan,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale awaiting-input sessions: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale awaiting-input session id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListEvents returns every event for sessionID in seq order — the full
// history Hygiene and promptctx.Build work from. RLS-scoped like every read
// in this package.
func ListEvents(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) ([]eventlog.Event, error) {
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

	var events []eventlog.Event
	for rows.Next() {
		var e eventlog.Event
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
