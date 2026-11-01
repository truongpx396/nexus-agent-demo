// Package postgres is the Postgres adapter for internal/teams: the teams, board
// cards and team_envelopes tables, with the session, key and event writes a
// team's creation, board activity and ending make in the same transaction. The
// rules — the depth bound, when a team is complete, what ending a team
// appends — are teams' own.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
)

// Store implements teams.Store over Postgres.
type Store struct {
	st     *store.Store
	sealer *sealed.Sealer
}

var _ teams.Store = (*Store)(nil)

// New returns a Store. keys seals each event under the session's own key and
// mints a member's key; chain may be nil, in which case events are appended
// without a signed receipt (a minimal wiring for tests).
func New(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain) *Store {
	return &Store{st: st, sealer: sealed.NewSealer(keys, chain)}
}

// InTenant implements teams.Store.
func (s *Store) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx teams.Tx) error) error {
	return s.sealer.InTenant(ctx, s.st, tenantID, func(ctx context.Context, u *sealed.Unit) error {
		return fn(ctx, &txn{Unit: u})
	})
}

// txn is the teams.Tx over one pgx transaction.
type txn struct{ *sealed.Unit }

var _ teams.Tx = (*txn)(nil)

type row interface {
	Scan(dest ...any) error
}

const teamSelectSQL = `
	SELECT team_id, tenant_id, name, coordinator_session_id, roster,
	       envelope_id, status, reason, created_at, completed_at
	FROM teams`

func scanTeam(r row) (teams.Team, error) {
	var t teams.Team
	var roster []byte
	var reason *string
	err := r.Scan(
		&t.TeamID, &t.TenantID, &t.Name, &t.CoordinatorSessionID, &roster,
		&t.EnvelopeID, &t.Status, &reason, &t.CreatedAt, &t.CompletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return teams.Team{}, fmt.Errorf("%w: team", teams.ErrNotFound)
		}
		return teams.Team{}, fmt.Errorf("teams: scan team: %w", err)
	}
	t.Roster, err = unmarshalRoster(roster)
	if err != nil {
		return teams.Team{}, fmt.Errorf("teams: unmarshal roster: %w", err)
	}
	if reason != nil {
		t.Reason = *reason
	}
	return t, nil
}

// insertTeam creates a new team row in the 'active' status — the roster is
// fixed at this INSERT and never updated afterward (task 9.1's own "no
// mid-run recruitment").
func (t *txn) InsertTeam(ctx context.Context, teamID, tenantID uuid.UUID, name string, coordinatorSessionID uuid.UUID, roster []teams.MemberSpec) error {
	rosterJSON, err := marshalRoster(roster)
	if err != nil {
		return fmt.Errorf("teams: marshal roster: %w", err)
	}
	_, err = t.Tx().Exec(ctx, `
		INSERT INTO teams (team_id, tenant_id, name, coordinator_session_id, roster)
		VALUES ($1,$2,$3,$4,$5)`,
		teamID, tenantID, name, coordinatorSessionID, rosterJSON,
	)
	if err != nil {
		return fmt.Errorf("teams: insert team: %w", err)
	}
	return nil
}

func (t *txn) GetTeam(ctx context.Context, teamID uuid.UUID) (teams.Team, error) {
	return scanTeam(t.Tx().QueryRow(ctx, teamSelectSQL+` WHERE team_id = $1`, teamID))
}

func (t *txn) SetTeamEnvelope(ctx context.Context, teamID, envelopeID uuid.UUID) error {
	tag, err := t.Tx().Exec(ctx, `UPDATE teams SET envelope_id = $2 WHERE team_id = $1`, teamID, envelopeID)
	if err != nil {
		return fmt.Errorf("teams: bind envelope to team %s: %w", teamID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("teams: bind envelope to team %s: no such team", teamID)
	}
	return nil
}

// endTeamIfActive is the ONE compare-and-set that ever moves a team out of
// 'active' (task 9.9): a plain conditional UPDATE, not an explicit row lock
// plus a surrounding transaction — the same "refuse rather than go
// negative" idiom internal/delegate/envelope.go's own drawFromEnvelope
// already uses for the analogous race. Exactly one caller, of however many
// call OnMemberTerminal/SweepBackstop concurrently for the same team, ever
// gets ok=true; every other caller (including a second trigger firing a
// heartbeat later for an already-ended team) sees ok=false and does
// nothing further — the same "second call is harmless" convention
// internal/runctl.Control.Cancel already documents.
func (t *txn) EndTeamIfActive(ctx context.Context, teamID uuid.UUID, status teams.Status, reason string) (coordinatorSessionID uuid.UUID, ok bool, err error) {
	err = t.Tx().QueryRow(ctx, `
		UPDATE teams SET status = $2, reason = $3, completed_at = now()
		WHERE team_id = $1 AND status = $4
		RETURNING coordinator_session_id`,
		teamID, status, reason, teams.StatusActive,
	).Scan(&coordinatorSessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil // already ended (by this call or a concurrent one) — not an error
		}
		return uuid.Nil, false, fmt.Errorf("teams: end team %s: %w", teamID, err)
	}
	return coordinatorSessionID, true, nil
}

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
	err = t.Tx().QueryRow(ctx, `
		SELECT count(*) FROM sessions WHERE team_id = $1 AND status NOT IN ('completed', 'failed')`,
		teamID,
	).Scan(&activeMembers)
	if err != nil {
		return 0, 0, fmt.Errorf("teams: count active members for team %s: %w", teamID, err)
	}
	return openOrClaimed, activeMembers, nil
}

// listActiveMemberSessionIDs is endTeam's own reaping query (task 9.9,
// reusing 8.14's own reaping discipline): every one of this team's member
// sessions that has not yet reached a terminal status.
func (t *txn) ListActiveMemberSessionIDs(ctx context.Context, teamID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := t.Tx().Query(ctx, `
		SELECT session_id FROM sessions WHERE team_id = $1 AND status NOT IN ('completed', 'failed')`,
		teamID,
	)
	if err != nil {
		return nil, fmt.Errorf("teams: list active members for team %s: %w", teamID, err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("teams: scan active member: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// listStaleActiveTeamIDs is the wall-clock backstop's own query (task 9.9):
// every 'active' team in tenantID whose created_at is older than the
// backstop window — SweepBackstop's caller decides how often to ask.
func (t *txn) ListStaleActiveTeamIDs(ctx context.Context, tenantID uuid.UUID, olderThan time.Time) ([]uuid.UUID, error) {
	rows, err := t.Tx().Query(ctx, `
		SELECT team_id FROM teams WHERE tenant_id = $1 AND status = $2 AND created_at < $3`,
		tenantID, teams.StatusActive, olderThan,
	)
	if err != nil {
		return nil, fmt.Errorf("teams: list stale active teams: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("teams: scan stale team id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

const cardSelectSQL = `
	SELECT card_id, tenant_id, team_id, title, body, status, taint_state,
	       injection_scan_status, scan_findings, written_by_session_id,
	       claimed_by_session_id, created_at, updated_at
	FROM board_cards`

func scanCard(r row) (teams.Card, error) {
	var c teams.Card
	var taint, findings []byte
	err := r.Scan(
		&c.CardID, &c.TenantID, &c.TeamID, &c.Title, &c.Body, &c.Status, &taint,
		&c.ScanStatus, &findings, &c.WrittenBySessionID,
		&c.ClaimedBySessionID, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return teams.Card{}, fmt.Errorf("%w: card", teams.ErrNotFound)
		}
		return teams.Card{}, fmt.Errorf("teams: scan card: %w", err)
	}
	c.TaintState, err = unmarshalTaint(taint)
	if err != nil {
		return teams.Card{}, fmt.Errorf("teams: unmarshal taint_state: %w", err)
	}
	if len(findings) > 0 {
		if err := json.Unmarshal(findings, &c.ScanFindings); err != nil {
			return teams.Card{}, fmt.Errorf("teams: unmarshal scan_findings: %w", err)
		}
	}
	return c, nil
}

// insertCard writes one new card — write_card never edits an existing row
// (task 9.3's own "copied from the writer at creation" is only meaningful
// for a row that never changes hands mid-content).
func (t *txn) InsertCard(ctx context.Context, tenantID, teamID uuid.UUID, title, body string, writtenBy uuid.UUID, taint [3]bool, scan teams.ScanStatus, findings []string) (teams.Card, error) {
	taintJSON, err := marshalTaint(taint)
	if err != nil {
		return teams.Card{}, fmt.Errorf("teams: marshal taint_state: %w", err)
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return teams.Card{}, fmt.Errorf("teams: marshal scan_findings: %w", err)
	}
	cardID := uuid.New()
	var createdAt, updatedAt time.Time
	err = t.Tx().QueryRow(ctx, `
		INSERT INTO board_cards (card_id, tenant_id, team_id, title, body, taint_state, injection_scan_status, scan_findings, written_by_session_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING created_at, updated_at`,
		cardID, tenantID, teamID, title, body, taintJSON, scan, findingsJSON, writtenBy,
	).Scan(&createdAt, &updatedAt)
	if err != nil {
		return teams.Card{}, fmt.Errorf("teams: insert card: %w", err)
	}
	return teams.Card{
		CardID: cardID, TenantID: tenantID, TeamID: teamID, Title: title, Body: body,
		Status: teams.CardOpen, TaintState: taint, ScanStatus: scan, ScanFindings: findings,
		WrittenBySessionID: writtenBy, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func (t *txn) ListCards(ctx context.Context, teamID uuid.UUID) ([]teams.Card, error) {
	rows, err := t.Tx().Query(ctx, cardSelectSQL+` WHERE team_id = $1 ORDER BY created_at ASC`, teamID)
	if err != nil {
		return nil, fmt.Errorf("teams: list cards for team %s: %w", teamID, err)
	}
	defer rows.Close()
	var out []teams.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// claimCard is task 9.4's own claim query — the SAME shape
// internal/queue/postgres.go's own Lease already runs for job dispatch
// (task 6.1): SELECT ... FOR UPDATE SKIP LOCKED, then UPDATE inside the
// SAME short transaction. Two members racing to claim the SAME card_id
// concurrently can never both walk away with it: whichever transaction's
// SELECT wins the row lock proceeds to the UPDATE and commits; the other
// either blocks and then finds status no longer 'open' (an ordinary
// UPDATE-lost-the-race, WHERE-clause-no-longer-matches outcome) or, if it
// arrives while the first is still mid-transaction, SKIP LOCKED simply
// excludes the row from its own SELECT and it sees claimed=false
// immediately rather than blocking at all.
func (t *txn) ClaimCard(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID) (card teams.Card, claimed bool, err error) {
	// injection_scan_status = 'clean' is a deliberate second gate alongside
	// status = 'open' (task 9.7): a flagged card is never surfaced to
	// another peer's context, including through a claim that never shows
	// the body via read_board first.
	row := t.Tx().QueryRow(ctx, cardSelectSQL+`
		WHERE card_id = $1 AND team_id = $2 AND tenant_id = $3 AND status = $4 AND injection_scan_status = $5
		FOR UPDATE SKIP LOCKED`,
		cardID, teamID, tenantID, teams.CardOpen, teams.ScanClean,
	)
	c, err := scanCard(row)
	if err != nil {
		if errors.Is(err, teams.ErrNotFound) {
			return teams.Card{}, false, nil // no such open, unlocked card — either already claimed, contended, or unknown
		}
		return teams.Card{}, false, err
	}
	tag, err := t.Tx().Exec(ctx, `
		UPDATE board_cards SET status = $2, claimed_by_session_id = $3, updated_at = now()
		WHERE card_id = $1`,
		cardID, teams.CardClaimed, sessionID,
	)
	if err != nil {
		return teams.Card{}, false, fmt.Errorf("teams: claim card %s: %w", cardID, err)
	}
	if tag.RowsAffected() == 0 {
		return teams.Card{}, false, nil
	}
	c.Status, c.ClaimedBySessionID = teams.CardClaimed, &sessionID
	return c, true, nil
}

// updateCardStatus enforces task 9.5's own bound: only the session that
// currently holds the claim may move it, and only into one of the three
// post-claim states — moving OUT of 'open' is claimCard's job alone, never
// this method's.
func (t *txn) UpdateCardStatus(ctx context.Context, teamID, cardID, claimantSessionID uuid.UUID, status teams.CardStatus) (teams.Card, bool, error) {
	tag, err := t.Tx().Exec(ctx, `
		UPDATE board_cards SET status = $4, updated_at = now()
		WHERE card_id = $1 AND team_id = $2 AND claimed_by_session_id = $3`,
		cardID, teamID, claimantSessionID, status,
	)
	if err != nil {
		return teams.Card{}, false, fmt.Errorf("teams: update card %s status: %w", cardID, err)
	}
	if tag.RowsAffected() == 0 {
		return teams.Card{}, false, nil // not claimed by this session (or does not exist) — fail closed, not an error
	}
	c, err := scanCard(t.Tx().QueryRow(ctx, cardSelectSQL+` WHERE card_id = $1`, cardID))
	return c, true, err
}

// teamIDForSession resolves sessions.team_id — the ONLY way a board tool
// ever learns which team it may touch (never trusted from a tool's own
// input), so a session can act on nothing but its own team's board by
// construction.
func (t *txn) TeamIDForSession(ctx context.Context, sessionID uuid.UUID) (teamID uuid.UUID, ok bool, err error) {
	var id *uuid.UUID
	err = t.Tx().QueryRow(ctx, `SELECT team_id FROM sessions WHERE session_id = $1`, sessionID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("teams: resolve team for session %s: %w", sessionID, err)
	}
	if id == nil {
		return uuid.Nil, false, nil
	}
	return *id, true, nil
}

// createEnvelope reserves a team's whole-roster ceiling BEFORE the first
// member starts (task 9.8) — the same shape internal/delegate/envelope.go's
// own CreateEnvelope already proves for a delegate_fanout step (task 8.13),
// scoped here to team_envelopes instead of fanout_envelopes: a plain
// Postgres row, never internal/cost's tenant-scoped Redis counter. Every
// member's own turn-level draw (EnvelopeBudgetGate.Reserve, below)
// decrements THIS row instead, which is what makes "no member draws an
// independent per-tenant reservation" true by construction.
func (t *txn) CreateEnvelope(ctx context.Context, tenantID, teamID uuid.UUID, ceiling cost.Money, memberCount int) (uuid.UUID, error) {
	envelopeID := uuid.New()
	_, err := t.Tx().Exec(ctx, `
		INSERT INTO team_envelopes (envelope_id, tenant_id, team_id, currency, ceiling_micros, remaining_micros, member_count)
		VALUES ($1,$2,$3,$4,$5,$5,$6)`,
		envelopeID, tenantID, teamID, ceiling.Currency, ceiling.Micros, memberCount,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("teams: create envelope: %w", err)
	}
	return envelopeID, nil
}

// drawFromEnvelope atomically decrements remaining_micros by amount,
// refusing (ok=false) rather than going negative — mirrors
// internal/delegate/envelope.go's own drawFromEnvelope exactly; the CHECK
// (remaining_micros >= 0) constraint backs this up at the schema level too,
// but the conditional UPDATE is what turns "refuse" into a normal, expected
// outcome instead of a constraint-violation error.
func (t *txn) DrawFromEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) (ok bool, remaining cost.Money, err error) {
	var remainingMicros int64
	var currency string
	err = t.Tx().QueryRow(ctx, `
		UPDATE team_envelopes SET remaining_micros = remaining_micros - $2
		WHERE envelope_id = $1 AND remaining_micros >= $2
		RETURNING remaining_micros, currency`,
		envelopeID, amount.Micros,
	).Scan(&remainingMicros, &currency)
	if err != nil {
		if err == pgx.ErrNoRows {
			var cur int64
			var cy string
			if qerr := t.Tx().QueryRow(ctx, `SELECT remaining_micros, currency FROM team_envelopes WHERE envelope_id = $1`, envelopeID).Scan(&cur, &cy); qerr != nil {
				return false, cost.Money{}, fmt.Errorf("teams: envelope %s not found", envelopeID)
			}
			return false, cost.Money{Micros: cur, Currency: cy}, nil
		}
		return false, cost.Money{}, fmt.Errorf("teams: draw from envelope %s: %w", envelopeID, err)
	}
	return true, cost.Money{Micros: remainingMicros, Currency: currency}, nil
}

func marshalRoster(roster []teams.MemberSpec) ([]byte, error) { return json.Marshal(roster) }

func unmarshalRoster(b []byte) ([]teams.MemberSpec, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []teams.MemberSpec
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func marshalTaint(engaged [3]bool) ([]byte, error) { return json.Marshal(engaged) }

func unmarshalTaint(b []byte) ([3]bool, error) {
	var out [3]bool
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (t *txn) ReturnToEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) error {
	_, err := t.Tx().Exec(ctx, `UPDATE team_envelopes SET remaining_micros = remaining_micros + $2 WHERE envelope_id = $1`, envelopeID, amount.Micros)
	return err
}

func (t *txn) EnvelopeShare(ctx context.Context, envelopeID uuid.UUID) (cost.Money, int, error) {
	var ceilingMicros int64
	var currency string
	var memberCount int
	err := t.Tx().QueryRow(ctx, `SELECT ceiling_micros, currency, member_count FROM team_envelopes WHERE envelope_id = $1`, envelopeID).
		Scan(&ceilingMicros, &currency, &memberCount)
	if err != nil {
		return cost.Money{}, 0, fmt.Errorf("teams: load envelope %s: %w", envelopeID, err)
	}
	return cost.Money{Micros: ceilingMicros, Currency: currency}, memberCount, nil
}
