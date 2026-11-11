package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	autonomy := s.AutonomyLevel
	if autonomy == "" {
		autonomy = "supervised"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO sessions (
			session_id, session_key, tenant_id, surface_id, user_id,
			agent_id, agent_version, harness_digest,
			data_label, route_model_id, route_reason,
			autonomy_level, parent_session_id, conversational
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		s.SessionID, s.SessionKey, s.TenantID, s.SurfaceID, s.UserID,
		s.AgentID, s.AgentVersion, s.HarnessDigest,
		s.DataLabel, s.RouteModelID, reason,
		autonomy, s.ParentSessionID, s.Conversational,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// ErrSessionNotFound is what GetSession wraps when no row is visible. It is
// eventlog.ErrSessionNotFound, so both names match under errors.Is.
var ErrSessionNotFound = eventlog.ErrSessionNotFound

// sessionColumns is every column the session reads select, in the order
// scanSession scans them: one list, so a column added to the aggregate is added
// in one place and cannot be read by one query and forgotten by another.
const sessionColumns = `session_id, session_key, tenant_id, surface_id, user_id,
	agent_id, agent_version, harness_digest,
	data_label, route_model_id, route_reason,
	autonomy_level, parent_session_id,
	status, terminal_reason, created_at, conversational, updated_at`

// scanSession reads one row selected with sessionColumns.
func scanSession(row interface{ Scan(dest ...any) error }) (eventlog.Session, error) {
	var s eventlog.Session
	var reason []byte
	if err := row.Scan(
		&s.SessionID, &s.SessionKey, &s.TenantID, &s.SurfaceID, &s.UserID,
		&s.AgentID, &s.AgentVersion, &s.HarnessDigest,
		&s.DataLabel, &s.RouteModelID, &reason,
		&s.AutonomyLevel, &s.ParentSessionID,
		&s.Status, &s.TerminalReason, &s.CreatedAt, &s.Conversational, &s.UpdatedAt,
	); err != nil {
		return eventlog.Session{}, err
	}
	if len(reason) > 0 {
		if err := json.Unmarshal(reason, &s.RouteReason); err != nil {
			return eventlog.Session{}, fmt.Errorf("unmarshal route_reason: %w", err)
		}
	}
	return s, nil
}

// GetSession loads one session row, RLS-scoped like every read in this
// package.
func GetSession(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (eventlog.Session, error) {
	s, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE session_id = $1`, sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return eventlog.Session{}, fmt.Errorf("session %s (wrong tenant scope, or it does not exist): %w", sessionID, ErrSessionNotFound)
		}
		return eventlog.Session{}, fmt.Errorf("get session %s: %w", sessionID, err)
	}
	return s, nil
}

// SessionAutonomy reads one session's autonomy level, RLS-scoped. The kernel asks
// before each batch of tool calls, so it reads the one column and not the row.
func SessionAutonomy(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var level string
	if err := tx.QueryRow(ctx, `SELECT autonomy_level FROM sessions WHERE session_id = $1`, sessionID).Scan(&level); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("session %s (wrong tenant scope, or it does not exist): %w", sessionID, ErrSessionNotFound)
		}
		return "", fmt.Errorf("read autonomy of session %s: %w", sessionID, err)
	}
	return level, nil
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
	s, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+`
		FROM sessions WHERE tenant_id = $1 AND session_key = $2
		ORDER BY created_at DESC LIMIT 1`, tenantID, sessionKey))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return eventlog.Session{}, false, nil
		}
		return eventlog.Session{}, false, fmt.Errorf("get session by key %q: %w", sessionKey, err)
	}
	return s, true, nil
}

// ListSessionsForUser returns userID's own top-level sessions (no parent -- a
// delegated or team-member session isn't its own thread; it's reached through
// its parent's own event log via child_session_id), newest first. RLS-scoped like every read in this package. Used by the web
// UI's session sidebar (internal/surfaces/rest's GET /v1/sessions) -- there
// is otherwise no "list runs" capability, only GetSession's lookup-by-id.
func ListSessionsForUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID, limit int) ([]eventlog.Session, error) {
	rows, err := tx.Query(ctx, `SELECT `+sessionColumns+`
		FROM sessions
		WHERE user_id = $1 AND parent_session_id IS NULL
		ORDER BY created_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions for user %s: %w", userID, err)
	}
	defer rows.Close()

	var out []eventlog.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions for user %s: %w", userID, err)
	}
	return out, nil
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
