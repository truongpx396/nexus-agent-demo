package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

	// PlanID/PlanVersion pin which orchestration_plans row (README §8,
	// Phase 8) this session's own event log is a run of — nil/nil for every
	// ordinary, non-plan-driven session. Pinned at session creation, exactly
	// like HarnessDigest, so a later plan edit can never retroactively
	// change what an in-flight run is executing (internal/plan/lifecycle.go
	// task 8.4's "in-flight runs finish on their version").
	PlanID      *uuid.UUID
	PlanVersion *int

	// ForkedFromSessionID/ForkSeq/ForkOverrides are the fork lineage columns
	// migrations/0002_sessions.sql seamed in at Phase 1, populated
	// meaningfully starting Phase 6 (README task 6.11,
	// internal/runctl.Fork). Nil/zero for every non-forked session — which
	// is every session before this phase.
	ForkedFromSessionID *uuid.UUID
	ForkSeq             *int64
	ForkOverrides       map[string]string

	// TeamID is the peer-team seam (README §9, Phase 9): nil for every
	// non-team session (every session before this phase, and every root/
	// delegation session after it). Set once at creation for a
	// delegation_role="team_member" session and never updated afterward —
	// the roster is fixed at team creation (internal/teams task 9.1).
	TeamID *uuid.UUID

	// CreatedAt is read-only (the column's own DEFAULT now() -- CreateSession
	// never sets it). Added for ListSessionsForUser's own sort/display need;
	// GetSession fills it too since it's cheap to select alongside every
	// other column already read there.
	CreatedAt time.Time

	// Conversational opts this session into kernel.RunConfig.Conversational
	// pause-not-terminate semantics (migrations/0023_conversational_
	// sessions.sql) -- pinned at creation, exactly like AutonomyLevel, and
	// read back on every internal/runctl.Control.ResumeConversation call to
	// rebuild RunConfig. False for every session before this migration.
	Conversational bool

	// UpdatedAt is written by UpdateSessionStatus on every status
	// transition -- the idle-conversation sweep's own signal
	// (cmd/nexusd/background.go) for how long a session has sat in
	// SessionStatusAwaitingInput with nobody answering. Read-only here
	// (CreateSession never sets it; the column's own DEFAULT now() does).
	UpdatedAt time.Time
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
func GetSessionByKey(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, sessionKey string) (Session, bool, error) {
	var s Session
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
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("get session by key %q: %w", sessionKey, err)
	}
	if len(reason) > 0 {
		if err := json.Unmarshal(reason, &s.RouteReason); err != nil {
			return Session{}, false, fmt.Errorf("unmarshal route_reason: %w", err)
		}
	}
	if len(overrides) > 0 {
		if err := json.Unmarshal(overrides, &s.ForkOverrides); err != nil {
			return Session{}, false, fmt.Errorf("unmarshal fork_overrides: %w", err)
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
func ListSessionsForUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID, limit int) ([]Session, error) {
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

	var out []Session
	for rows.Next() {
		var s Session
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
		SessionStatusAwaitingInput, olderThan,
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
