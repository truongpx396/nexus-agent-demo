package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
)

// The roster: the team_members table, and the completion predicate and reaping
// query that read it.

// boardAndMemberCounts is task 9.9's own completion predicate, read fresh
// every time rather than cached: how many cards are still open/claimed, and
// how many of the roster's own sessions have not yet reached a terminal
// status.
func (t *txn) BoardAndMemberCounts(ctx context.Context, teamID uuid.UUID) (openOrClaimed, activeMembers int, err error) {
	err = t.Tx().QueryRow(ctx, `
		SELECT count(*) FROM board_cards WHERE team_id = $1 AND status IN ('open', 'claimed')`,
		teamID,
	).Scan(&openOrClaimed)
	if err != nil {
		return 0, 0, fmt.Errorf("teams: count open/claimed cards for team %s: %w", teamID, err)
	}
	active, err := t.ListActiveMemberSessionIDs(ctx, teamID)
	if err != nil {
		return 0, 0, err
	}
	return openOrClaimed, len(active), nil
}

// listActiveMemberSessionIDs is endTeam's own reaping query (task 9.9,
// reusing 8.14's own reaping discipline): every one of this team's member
// sessions that has not yet reached a terminal status. The roster is this
// package's own table; a member's status is the session aggregate's, so it is
// read through the aggregate (the roster is fixed at creation and small) rather
// than by joining the sessions table here.
func (t *txn) ListActiveMemberSessionIDs(ctx context.Context, teamID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := t.Tx().Query(ctx, `SELECT session_id FROM team_members WHERE team_id = $1 ORDER BY created_at`, teamID)
	if err != nil {
		return nil, fmt.Errorf("teams: list members for team %s: %w", teamID, err)
	}
	var members []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("teams: scan member: %w", err)
		}
		members = append(members, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: list members for team %s: %w", teamID, err)
	}

	var active []uuid.UUID
	for _, id := range members {
		sess, err := t.GetSession(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("teams: read member %s of team %s: %w", id, teamID, err)
		}
		if sess.Status != eventlog.SessionStatusCompleted && sess.Status != eventlog.SessionStatusFailed {
			active = append(active, id)
		}
	}
	return active, nil
}

// AddMember records a session on a team's roster.
func (t *txn) AddMember(ctx context.Context, tenantID, teamID, sessionID uuid.UUID) error {
	if _, err := t.Tx().Exec(ctx,
		`INSERT INTO team_members (session_id, tenant_id, team_id) VALUES ($1, $2, $3)`,
		sessionID, tenantID, teamID,
	); err != nil {
		return fmt.Errorf("teams: add member %s to team %s: %w", sessionID, teamID, err)
	}
	return nil
}

// teamIDForSession resolves the team a session is a member of — the ONLY way a board tool
// ever learns which team it may touch (never trusted from a tool's own
// input), so a session can act on nothing but its own team's board by
// construction.
func (t *txn) TeamIDForSession(ctx context.Context, sessionID uuid.UUID) (teamID uuid.UUID, ok bool, err error) {
	var id uuid.UUID
	err = t.Tx().QueryRow(ctx, `SELECT team_id FROM team_members WHERE session_id = $1`, sessionID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("teams: resolve team for session %s: %w", sessionID, err)
	}
	return id, true, nil
}
