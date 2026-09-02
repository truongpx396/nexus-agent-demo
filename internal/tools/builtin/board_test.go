package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

type fakeTeamResolver struct {
	teamID uuid.UUID
	ok     bool
	err    error
}

func (f fakeTeamResolver) TeamIDFor(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, bool, error) {
	return f.teamID, f.ok, f.err
}

func TestResolveTeam_DeniesWhenResolverNotWired(t *testing.T) {
	_, res := resolveTeam(context.Background(), nil, tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (fail closed with no resolver wired)", res.Decision)
	}
}

func TestResolveTeam_DeniesNonMember(t *testing.T) {
	_, res := resolveTeam(context.Background(), fakeTeamResolver{ok: false}, tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (a session with no team_id may not touch any board)", res.Decision)
	}
}

func TestResolveTeam_DeniesOnLookupError(t *testing.T) {
	_, res := resolveTeam(context.Background(), fakeTeamResolver{err: errors.New("boom")}, tools.RunContext{})
	if res.Decision != "deny" {
		t.Fatalf("decision = %q, want deny (fail closed on a lookup error)", res.Decision)
	}
}

func TestResolveTeam_DefersForAMember(t *testing.T) {
	teamID := uuid.New()
	got, res := resolveTeam(context.Background(), fakeTeamResolver{teamID: teamID, ok: true}, tools.RunContext{})
	if res.Decision != "defer" {
		t.Fatalf("decision = %q, want defer (Gate 2 must never resolve allow — README.md §4)", res.Decision)
	}
	if got != teamID {
		t.Fatalf("resolved team = %s, want %s", got, teamID)
	}
}

func TestBoardTools_Taint_DefaultsAllThreeLegsTrue(t *testing.T) {
	for name, taint := range map[string]tools.Taint{
		"ReadBoard":        ReadBoard{}.Taint(),
		"ClaimCard":        ClaimCard{}.Taint(),
		"WriteCard":        WriteCard{}.Taint(),
		"UpdateCardStatus": UpdateCardStatus{}.Taint(),
	} {
		if !taint.ReturnsUntrusted || !taint.ReadsPrivateData || !taint.MutatesExternal {
			t.Fatalf("%s.Taint() = %+v, want every leg true (README task 9.5)", name, taint)
		}
	}
}

func TestBoardTools_CheckPermissions_NeverResolvesAllow(t *testing.T) {
	rc := tools.RunContext{TenantID: uuid.New(), SessionID: uuid.New()}
	resolver := fakeTeamResolver{teamID: uuid.New(), ok: true}
	for name, decision := range map[string]string{
		"ReadBoard":        ReadBoard{Resolver: resolver}.CheckPermissions(context.Background(), nil, rc).Decision,
		"ClaimCard":        ClaimCard{Resolver: resolver}.CheckPermissions(context.Background(), nil, rc).Decision,
		"WriteCard":        WriteCard{Resolver: resolver}.CheckPermissions(context.Background(), nil, rc).Decision,
		"UpdateCardStatus": UpdateCardStatus{Resolver: resolver}.CheckPermissions(context.Background(), nil, rc).Decision,
	} {
		if decision == "allow" {
			t.Fatalf("%s.CheckPermissions() = allow, want deny/defer only (Gate 2 must never resolve allow)", name)
		}
	}
}

func TestClaimCard_ValidateInput_RejectsMissingOrInvalidCardID(t *testing.T) {
	c := ClaimCard{}
	if err := c.ValidateInput(context.Background(), json.RawMessage(`{}`), tools.RunContext{}); err == nil {
		t.Fatalf("ValidateInput accepted a missing card_id")
	}
	if err := c.ValidateInput(context.Background(), json.RawMessage(`{"card_id":"not-a-uuid"}`), tools.RunContext{}); err == nil {
		t.Fatalf("ValidateInput accepted a non-uuid card_id")
	}
	if err := c.ValidateInput(context.Background(), json.RawMessage(`{"card_id":"`+uuid.New().String()+`"}`), tools.RunContext{}); err != nil {
		t.Fatalf("ValidateInput rejected a valid card_id: %v", err)
	}
}

type fakeClaimer struct {
	card    BoardCard
	claimed bool
	err     error
	gotCard uuid.UUID
}

func (f *fakeClaimer) ClaimCard(_ context.Context, _, _, cardID, _ uuid.UUID) (BoardCard, bool, error) {
	f.gotCard = cardID
	return f.card, f.claimed, f.err
}

func TestClaimCard_Call_ReturnsErrorWhenNotClaimed(t *testing.T) {
	claimer := &fakeClaimer{claimed: false}
	c := ClaimCard{Resolver: fakeTeamResolver{teamID: uuid.New(), ok: true}, Claimer: claimer}
	in, _ := json.Marshal(claimCardInput{CardID: uuid.New().String()})
	out, err := c.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !out.IsError {
		t.Fatalf("Call() did not report IsError for a lost claim race")
	}
}

func TestClaimCard_Call_Success(t *testing.T) {
	cardID := uuid.New()
	claimer := &fakeClaimer{card: BoardCard{CardID: cardID.String(), Status: "claimed"}, claimed: true}
	c := ClaimCard{Resolver: fakeTeamResolver{teamID: uuid.New(), ok: true}, Claimer: claimer}
	in, _ := json.Marshal(claimCardInput{CardID: cardID.String()})
	out, err := c.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.IsError {
		t.Fatalf("Call() reported IsError for a successful claim: %s", out.Reason)
	}
	if claimer.gotCard != cardID {
		t.Fatalf("Claimer got card %s, want %s", claimer.gotCard, cardID)
	}
}

type fakeWriter struct {
	card BoardCard
	err  error
	got  WriteCardRequest
}

func (f *fakeWriter) WriteCard(_ context.Context, req WriteCardRequest) (BoardCard, error) {
	f.got = req
	return f.card, f.err
}

func TestWriteCard_ValidateInput_RequiresTitle(t *testing.T) {
	w := WriteCard{}
	if err := w.ValidateInput(context.Background(), json.RawMessage(`{"body":"x"}`), tools.RunContext{}); err == nil {
		t.Fatalf("ValidateInput accepted a missing title")
	}
}

func TestWriteCard_Call_PassesThroughToWriter(t *testing.T) {
	writer := &fakeWriter{card: BoardCard{CardID: uuid.New().String(), Status: "open"}}
	sessionID, tenantID := uuid.New(), uuid.New()
	w := WriteCard{Resolver: fakeTeamResolver{teamID: uuid.New(), ok: true}, Writer: writer}
	in, _ := json.Marshal(writeCardInput{Title: "t", Body: "b"})
	out, err := w.Call(context.Background(), in, tools.RunContext{TenantID: tenantID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.IsError {
		t.Fatalf("Call() reported IsError: %s", out.Reason)
	}
	if writer.got.WrittenBySessionID != sessionID || writer.got.TenantID != tenantID {
		t.Fatalf("writer got %+v, want session %s tenant %s", writer.got, sessionID, tenantID)
	}
}

func TestUpdateCardStatus_ValidateInput_RejectsUnlistedStatus(t *testing.T) {
	u := UpdateCardStatus{}
	in, _ := json.Marshal(updateCardStatusInput{CardID: uuid.New().String(), Status: "open"})
	if err := u.ValidateInput(context.Background(), in, tools.RunContext{}); err == nil {
		t.Fatalf("ValidateInput accepted status=open — moving OUT of open is claim_card's job alone")
	}
}

func TestUpdateCardStatus_ValidateInput_AcceptsEachAllowedStatus(t *testing.T) {
	u := UpdateCardStatus{}
	for status := range allowedCardStatusUpdates {
		in, _ := json.Marshal(updateCardStatusInput{CardID: uuid.New().String(), Status: status})
		if err := u.ValidateInput(context.Background(), in, tools.RunContext{}); err != nil {
			t.Fatalf("ValidateInput rejected allowed status %q: %v", status, err)
		}
	}
}

type fakeUpdater struct {
	card BoardCard
	ok   bool
	err  error
}

func (f *fakeUpdater) UpdateCardStatus(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, string) (BoardCard, bool, error) {
	return f.card, f.ok, f.err
}

func TestUpdateCardStatus_Call_ReturnsErrorWhenNotClaimedByCaller(t *testing.T) {
	u := UpdateCardStatus{Resolver: fakeTeamResolver{teamID: uuid.New(), ok: true}, Updater: &fakeUpdater{ok: false}}
	in, _ := json.Marshal(updateCardStatusInput{CardID: uuid.New().String(), Status: "done"})
	out, err := u.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !out.IsError {
		t.Fatalf("Call() did not report IsError when the card is not claimed by this session")
	}
}

type fakeReader struct {
	cards []BoardCard
	err   error
}

func (f *fakeReader) ReadBoard(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]BoardCard, error) {
	return f.cards, f.err
}

func TestReadBoard_Call_ReturnsCards(t *testing.T) {
	reader := &fakeReader{cards: []BoardCard{{CardID: uuid.New().String(), Title: "a"}, {CardID: uuid.New().String(), Title: "b"}}}
	r := ReadBoard{Resolver: fakeTeamResolver{teamID: uuid.New(), ok: true}, Reader: reader}
	out, err := r.Call(context.Background(), nil, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.IsError {
		t.Fatalf("Call() reported IsError: %s", out.Reason)
	}
	var parsed struct {
		Cards []BoardCard `json:"cards"`
	}
	if err := json.Unmarshal(out.Output, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(parsed.Cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(parsed.Cards))
	}
}
