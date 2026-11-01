package teams_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
)

// fakeStore is a teams.Store over maps. Like a real transaction it snapshots on
// entry and restores on error, so an operation that fails part-way leaves
// nothing behind — which is what lets these tests pin that a team's status and
// the event that records it only ever change together.
type fakeStore struct {
	mu sync.Mutex

	sessions  map[uuid.UUID]eventlog.Session
	teams     map[uuid.UUID]teams.Team
	cards     map[uuid.UUID]teams.Card
	envelopes map[uuid.UUID]*envelope
	events    []appended

	failAppend error
}

type envelope struct {
	ceiling, remaining cost.Money
	members            int
}

type appended struct {
	SessionID uuid.UUID
	Type      eventlog.EventType
	Payload   any
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[uuid.UUID]eventlog.Session{}, teams: map[uuid.UUID]teams.Team{},
		cards: map[uuid.UUID]teams.Card{}, envelopes: map[uuid.UUID]*envelope{},
	}
}

var _ teams.Store = (*fakeStore)(nil)

func (f *fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(ctx context.Context, tx teams.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sessions, tms, cards, events := map[uuid.UUID]eventlog.Session{}, map[uuid.UUID]teams.Team{}, map[uuid.UUID]teams.Card{}, append([]appended(nil), f.events...)
	envs := map[uuid.UUID]envelope{}
	for k, v := range f.sessions {
		sessions[k] = v
	}
	for k, v := range f.teams {
		tms[k] = v
	}
	for k, v := range f.cards {
		cards[k] = v
	}
	for k, v := range f.envelopes {
		envs[k] = *v
	}
	if err := fn(ctx, &fakeTx{f: f}); err != nil {
		f.sessions, f.teams, f.cards, f.events = sessions, tms, cards, events
		f.envelopes = map[uuid.UUID]*envelope{}
		for k, v := range envs {
			v := v
			f.envelopes[k] = &v
		}
		return err
	}
	return nil
}

func (f *fakeStore) eventTypes() []eventlog.EventType {
	var out []eventlog.EventType
	for _, e := range f.events {
		out = append(out, e.Type)
	}
	return out
}

// seedTeam makes an active team with a coordinator and n member sessions in the
// given status.
func (f *fakeStore) seedTeam(n int, memberStatus string) (teams.Team, []eventlog.Session) {
	coord := eventlog.Session{SessionID: uuid.New(), Status: eventlog.SessionStatusSuspended}
	f.sessions[coord.SessionID] = coord
	t := teams.Team{TeamID: uuid.New(), CoordinatorSessionID: coord.SessionID, Status: teams.StatusActive}
	f.teams[t.TeamID] = t
	var members []eventlog.Session
	for i := 0; i < n; i++ {
		tid := t.TeamID
		m := eventlog.Session{SessionID: uuid.New(), Status: memberStatus, TeamID: &tid}
		f.sessions[m.SessionID] = m
		members = append(members, m)
	}
	return t, members
}

func (f *fakeStore) seedCard(teamID uuid.UUID, status teams.CardStatus, scan teams.ScanStatus) teams.Card {
	c := teams.Card{CardID: uuid.New(), TeamID: teamID, Title: "t", Body: "b", Status: status, ScanStatus: scan}
	f.cards[c.CardID] = c
	return c
}

type fakeTx struct{ f *fakeStore }

var _ teams.Tx = (*fakeTx)(nil)

func (t *fakeTx) Append(_ context.Context, _, sessionID uuid.UUID, typ eventlog.EventType, _ *string, _ *uuid.UUID, payload any) (eventlog.Event, error) {
	if t.f.failAppend != nil {
		return eventlog.Event{}, t.f.failAppend
	}
	t.f.events = append(t.f.events, appended{SessionID: sessionID, Type: typ, Payload: payload})
	return eventlog.Event{EventID: uuid.New(), SessionID: sessionID, Type: typ}, nil
}

func (t *fakeTx) GetSession(_ context.Context, id uuid.UUID) (eventlog.Session, error) {
	s, ok := t.f.sessions[id]
	if !ok {
		return eventlog.Session{}, errors.New("no such session")
	}
	return s, nil
}
func (t *fakeTx) CreateSession(_ context.Context, s eventlog.Session) error {
	t.f.sessions[s.SessionID] = s
	return nil
}
func (t *fakeTx) SetSessionStatus(context.Context, uuid.UUID, string, *string) error { return nil }
func (t *fakeTx) ListEvents(context.Context, uuid.UUID) ([]eventlog.Event, error)    { return nil, nil }
func (t *fakeTx) NewDEK(context.Context, uuid.UUID) (crypto.DEK, error) {
	return crypto.GenerateDEK("k")
}
func (t *fakeTx) Unwrap(context.Context, string) (crypto.DEK, error) { return crypto.GenerateDEK("k") }

func (t *fakeTx) InsertTeam(_ context.Context, id, tenantID uuid.UUID, name string, coord uuid.UUID, roster []teams.MemberSpec) error {
	t.f.teams[id] = teams.Team{TeamID: id, TenantID: tenantID, Name: name, CoordinatorSessionID: coord, Roster: roster, Status: teams.StatusActive}
	return nil
}
func (t *fakeTx) GetTeam(_ context.Context, id uuid.UUID) (teams.Team, error) {
	tm, ok := t.f.teams[id]
	if !ok {
		return teams.Team{}, fmt.Errorf("%w: team", teams.ErrNotFound)
	}
	return tm, nil
}
func (t *fakeTx) SetTeamEnvelope(_ context.Context, id, envID uuid.UUID) error {
	tm := t.f.teams[id]
	tm.EnvelopeID = &envID
	t.f.teams[id] = tm
	return nil
}
func (t *fakeTx) EndTeamIfActive(_ context.Context, id uuid.UUID, status teams.Status, reason string) (uuid.UUID, bool, error) {
	tm, ok := t.f.teams[id]
	if !ok || tm.Status != teams.StatusActive {
		return uuid.Nil, false, nil
	}
	now := time.Now()
	tm.Status, tm.Reason, tm.CompletedAt = status, reason, &now
	t.f.teams[id] = tm
	return tm.CoordinatorSessionID, true, nil
}
func (t *fakeTx) BoardAndMemberCounts(_ context.Context, teamID uuid.UUID) (int, int, error) {
	open, active := 0, 0
	for _, c := range t.f.cards {
		if c.TeamID == teamID && (c.Status == teams.CardOpen || c.Status == teams.CardClaimed || c.Status == teams.CardInProgress) {
			open++
		}
	}
	for _, s := range t.f.sessions {
		if s.TeamID != nil && *s.TeamID == teamID && s.Status != eventlog.SessionStatusCompleted && s.Status != eventlog.SessionStatusFailed {
			active++
		}
	}
	return open, active, nil
}
func (t *fakeTx) ListActiveMemberSessionIDs(_ context.Context, teamID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, s := range t.f.sessions {
		if s.TeamID != nil && *s.TeamID == teamID && s.Status != eventlog.SessionStatusCompleted && s.Status != eventlog.SessionStatusFailed {
			out = append(out, s.SessionID)
		}
	}
	return out, nil
}
func (t *fakeTx) ListStaleActiveTeamIDs(_ context.Context, _ uuid.UUID, olderThan time.Time) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for id, tm := range t.f.teams {
		if tm.Status == teams.StatusActive && tm.CreatedAt.Before(olderThan) {
			out = append(out, id)
		}
	}
	return out, nil
}
func (t *fakeTx) TeamIDForSession(_ context.Context, id uuid.UUID) (uuid.UUID, bool, error) {
	s, ok := t.f.sessions[id]
	if !ok || s.TeamID == nil {
		return uuid.Nil, false, nil
	}
	return *s.TeamID, true, nil
}

func (t *fakeTx) InsertCard(_ context.Context, tenantID, teamID uuid.UUID, title, body string, writtenBy uuid.UUID, taint [3]bool, scan teams.ScanStatus, findings []string) (teams.Card, error) {
	c := teams.Card{CardID: uuid.New(), TenantID: tenantID, TeamID: teamID, Title: title, Body: body, Status: teams.CardOpen, TaintState: taint, ScanStatus: scan, ScanFindings: findings, WrittenBySessionID: writtenBy}
	t.f.cards[c.CardID] = c
	return c, nil
}
func (t *fakeTx) ListCards(_ context.Context, teamID uuid.UUID) ([]teams.Card, error) {
	var out []teams.Card
	for _, c := range t.f.cards {
		if c.TeamID == teamID {
			out = append(out, c)
		}
	}
	return out, nil
}
func (t *fakeTx) ClaimCard(_ context.Context, _, teamID, cardID, sessionID uuid.UUID) (teams.Card, bool, error) {
	c, ok := t.f.cards[cardID]
	if !ok || c.TeamID != teamID || c.Status != teams.CardOpen || c.ScanStatus != teams.ScanClean {
		return teams.Card{}, false, nil
	}
	c.Status, c.ClaimedBySessionID = teams.CardClaimed, &sessionID
	t.f.cards[cardID] = c
	return c, true, nil
}
func (t *fakeTx) UpdateCardStatus(_ context.Context, teamID, cardID, claimant uuid.UUID, status teams.CardStatus) (teams.Card, bool, error) {
	c, ok := t.f.cards[cardID]
	if !ok || c.TeamID != teamID || c.ClaimedBySessionID == nil || *c.ClaimedBySessionID != claimant {
		return teams.Card{}, false, nil
	}
	c.Status = status
	t.f.cards[cardID] = c
	return c, true, nil
}

func (t *fakeTx) CreateEnvelope(_ context.Context, _, _ uuid.UUID, ceiling cost.Money, members int) (uuid.UUID, error) {
	id := uuid.New()
	t.f.envelopes[id] = &envelope{ceiling: ceiling, remaining: ceiling, members: members}
	return id, nil
}
func (t *fakeTx) DrawFromEnvelope(_ context.Context, id uuid.UUID, amount cost.Money) (bool, cost.Money, error) {
	e, ok := t.f.envelopes[id]
	if !ok {
		return false, cost.Money{}, fmt.Errorf("envelope %s not found", id)
	}
	if e.remaining.Micros < amount.Micros {
		return false, e.remaining, nil
	}
	e.remaining.Micros -= amount.Micros
	return true, e.remaining, nil
}
func (t *fakeTx) ReturnToEnvelope(_ context.Context, id uuid.UUID, amount cost.Money) error {
	t.f.envelopes[id].remaining.Micros += amount.Micros
	return nil
}
func (t *fakeTx) EnvelopeShare(_ context.Context, id uuid.UUID) (cost.Money, int, error) {
	e, ok := t.f.envelopes[id]
	if !ok {
		return cost.Money{}, 0, errors.New("no such envelope")
	}
	return e.ceiling, e.members, nil
}
