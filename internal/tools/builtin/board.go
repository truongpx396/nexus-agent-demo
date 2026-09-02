package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

var (
	readBoardRef        = tools.ToolRef{Namespace: "platform", Name: "read_board", Version: "v1"}
	claimCardRef        = tools.ToolRef{Namespace: "platform", Name: "claim_card", Version: "v1"}
	writeCardRef        = tools.ToolRef{Namespace: "platform", Name: "write_card", Version: "v1"}
	updateCardStatusRef = tools.ToolRef{Namespace: "platform", Name: "update_card_status", Version: "v1"}
)

// BoardCard is this package's own view of one board_cards row —
// deliberately primitive-typed (no internal/teams.Card, no [3]bool taint
// state) so this package never needs to import internal/teams at all, the
// same decoupling SpawnRequest already gets from internal/delegate's own
// shape (internal/tools/builtin/delegate.go's own doc comment on why).
// Flagged, not Body, is what a caller checks: a flagged card's Body is
// already redacted to "" by the wiring layer before it ever reaches here
// (task 9.7 — never surfaced, not even as a non-empty-but-untrusted string
// this tool would have to remember not to trust).
type BoardCard struct {
	CardID             string
	Title              string
	Body               string
	Status             string
	Flagged            bool
	ClaimedBySessionID string // "" if unclaimed
}

// TeamResolver is the ONE lookup every board tool's CheckPermissions uses to
// decide which team a session may touch — never taken from the tool's own
// input (a card_id or team_id argument would be an attacker-controlled
// cross-team probe; there is no such argument anywhere in this file).
// Satisfied structurally by *internal/teams.Service.TeamIDFor.
type TeamResolver interface {
	TeamIDFor(ctx context.Context, tenantID, sessionID uuid.UUID) (teamID uuid.UUID, ok bool, err error)
}

// BoardReader is ReadBoard's own lookup — satisfied by
// *internal/teams.Service.ReadBoard via a thin adapter at the wiring layer
// (cmd/nexusd's own translation, the same shape nexusdDelegationSpawner
// already provides for Delegate).
type BoardReader interface {
	ReadBoard(ctx context.Context, tenantID, teamID, readerSessionID uuid.UUID) ([]BoardCard, error)
}

// CardClaimer is ClaimCard's own lookup.
type CardClaimer interface {
	ClaimCard(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID) (BoardCard, bool, error)
}

// WriteCardRequest is WriteCard's own view of what it asks the wiring layer
// to persist — mirrors internal/teams.WriteCardRequest's fields but stays
// this package's own type for the same reason BoardCard is.
type WriteCardRequest struct {
	TenantID           uuid.UUID
	TeamID             uuid.UUID
	Title              string
	Body               string
	WrittenBySessionID uuid.UUID
}

// CardWriter is WriteCard's own lookup.
type CardWriter interface {
	WriteCard(ctx context.Context, req WriteCardRequest) (BoardCard, error)
}

// CardStatusUpdater is UpdateCardStatus's own lookup.
type CardStatusUpdater interface {
	UpdateCardStatus(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID, status string) (BoardCard, bool, error)
}

// boardTaint is every board tool's declared Taint (README task 9.5: "Taint()
// defaults all-TRUE like every tool, so autonomy level and the Rule of Two
// gate board actions exactly as they gate delegate (8.9)") — a shared
// literal because all four tools take the identical fail-closed posture,
// never because they share a type.
func boardTaint() tools.Taint { return tools.DefaultTaint() }

// resolveTeam is the check every board tool's CheckPermissions runs first:
// a session with no team_id may not touch a board at all — Deny, never
// Ask, mirroring Gate 1's own "never resolves ALLOW, only DENY/DEFER"
// posture applied to a structural-membership check instead of a profile.
func resolveTeam(ctx context.Context, resolver TeamResolver, rc tools.RunContext) (uuid.UUID, tools.PermissionResult) {
	if resolver == nil {
		return uuid.Nil, tools.PermissionResult{Decision: "deny", Reason: "team resolver not wired (fail closed)"}
	}
	teamID, ok, err := resolver.TeamIDFor(ctx, rc.TenantID, rc.SessionID)
	if err != nil {
		return uuid.Nil, tools.PermissionResult{Decision: "deny", Reason: "team membership lookup failed (fail closed): " + err.Error()}
	}
	if !ok {
		return uuid.Nil, tools.PermissionResult{Decision: "deny", Reason: "session is not a member of any team"}
	}
	return teamID, tools.PermissionResult{Decision: "defer"}
}

// ReadBoard implements read_board(): lists every card on the caller's own
// team's board (README task 9.5). Read-only; folding a clean card's taint
// into the reader (task 9.6) happens inside BoardReader.ReadBoard itself
// (internal/teams.Service), not here — this tool is a thin adapter, the
// same split builtin.ActivateSkill keeps from internal/skills.Catalog.
type ReadBoard struct {
	Resolver TeamResolver
	Reader   BoardReader
}

func (ReadBoard) ID() tools.ToolRef { return readBoardRef }

func (ReadBoard) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          readBoardRef,
		Description: "Lists every card on the caller's own team's shared board.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		EffectClass: tools.EffectClassReadOnly,
	}
}

func (ReadBoard) Taint() tools.Taint { return boardTaint() }

func (ReadBoard) IsConcurrencySafe(json.RawMessage) bool { return true } // a read has nothing to race against

func (r ReadBoard) CheckPermissions(ctx context.Context, _ json.RawMessage, rc tools.RunContext) tools.PermissionResult {
	_, res := resolveTeam(ctx, r.Resolver, rc)
	return res
}

func (ReadBoard) ValidateInput(context.Context, json.RawMessage, tools.RunContext) error { return nil }

func (r ReadBoard) Call(ctx context.Context, _ json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	teamID, res := resolveTeam(ctx, r.Resolver, rc)
	if res.Decision == "deny" {
		return tools.Result{IsError: true, Reason: res.Reason}, nil
	}
	if r.Reader == nil {
		return tools.Result{IsError: true, Reason: "board reader not wired"}, nil
	}
	cards, err := r.Reader.ReadBoard(ctx, rc.TenantID, teamID, rc.SessionID)
	if err != nil {
		return tools.Result{IsError: true, Reason: "read_board failed: " + err.Error()}, nil
	}
	out, err := json.Marshal(map[string]any{"cards": cards})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}

// ClaimCard implements claim_card(card_id): task 9.4's own claim, atomic
// against every other session racing the same card_id — exactly one caller
// ever sees claimed=true for a given card (internal/teams.Service.ClaimCard's
// own SKIP LOCKED query is what makes that true, not anything in this file).
type ClaimCard struct {
	Resolver TeamResolver
	Claimer  CardClaimer
}

type claimCardInput struct {
	CardID string `json:"card_id"`
}

func (ClaimCard) ID() tools.ToolRef { return claimCardRef }

func (ClaimCard) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          claimCardRef,
		Description: "Claims one open card on the caller's own team's board by id. At most one session ever wins a race for the same card.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"card_id":{"type":"string"}},"required":["card_id"]}`),
		EffectClass: tools.EffectClassMutating,
	}
}

func (ClaimCard) Taint() tools.Taint { return boardTaint() }

func (ClaimCard) IsConcurrencySafe(json.RawMessage) bool { return false }

func (c ClaimCard) CheckPermissions(ctx context.Context, _ json.RawMessage, rc tools.RunContext) tools.PermissionResult {
	_, res := resolveTeam(ctx, c.Resolver, rc)
	return res
}

func (ClaimCard) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req claimCardInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.CardID == "" {
		return fmt.Errorf("card_id is required")
	}
	if _, err := uuid.Parse(req.CardID); err != nil {
		return fmt.Errorf("card_id is not a valid uuid: %w", err)
	}
	return nil
}

func (c ClaimCard) Call(ctx context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req claimCardInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	cardID, err := uuid.Parse(req.CardID)
	if err != nil {
		return tools.Result{}, err
	}
	teamID, res := resolveTeam(ctx, c.Resolver, rc)
	if res.Decision == "deny" {
		return tools.Result{IsError: true, Reason: res.Reason}, nil
	}
	if c.Claimer == nil {
		return tools.Result{IsError: true, Reason: "card claimer not wired"}, nil
	}
	card, claimed, err := c.Claimer.ClaimCard(ctx, rc.TenantID, teamID, cardID, rc.SessionID)
	if err != nil {
		return tools.Result{IsError: true, Reason: "claim_card failed: " + err.Error()}, nil
	}
	if !claimed {
		return tools.Result{IsError: true, Reason: "card_not_claimable: already claimed, not open, or not a clean card on this team's board"}, nil
	}
	out, err := json.Marshal(card)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}

// WriteCard implements write_card(title, body): creates a NEW card — it
// never edits an existing one (task 9.3's own "copied from the writer at
// creation" is only meaningful for a row that never changes hands
// mid-content). The body is scanned and the writer's own current taint
// captured inside CardWriter.WriteCard (internal/teams.Service), not here.
type WriteCard struct {
	Resolver TeamResolver
	Writer   CardWriter
}

type writeCardInput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (WriteCard) ID() tools.ToolRef { return writeCardRef }

func (WriteCard) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          writeCardRef,
		Description: "Writes a new card onto the caller's own team's shared board.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"body":{"type":"string"}},"required":["title","body"]}`),
		EffectClass: tools.EffectClassMutating,
	}
}

func (WriteCard) Taint() tools.Taint { return boardTaint() }

func (WriteCard) IsConcurrencySafe(json.RawMessage) bool { return false }

func (w WriteCard) CheckPermissions(ctx context.Context, _ json.RawMessage, rc tools.RunContext) tools.PermissionResult {
	_, res := resolveTeam(ctx, w.Resolver, rc)
	return res
}

func (WriteCard) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req writeCardInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.Title == "" {
		return fmt.Errorf("title is required")
	}
	return nil
}

func (w WriteCard) Call(ctx context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req writeCardInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	teamID, res := resolveTeam(ctx, w.Resolver, rc)
	if res.Decision == "deny" {
		return tools.Result{IsError: true, Reason: res.Reason}, nil
	}
	if w.Writer == nil {
		return tools.Result{IsError: true, Reason: "card writer not wired"}, nil
	}
	card, err := w.Writer.WriteCard(ctx, WriteCardRequest{
		TenantID: rc.TenantID, TeamID: teamID, Title: req.Title, Body: req.Body, WrittenBySessionID: rc.SessionID,
	})
	if err != nil {
		return tools.Result{IsError: true, Reason: "write_card failed: " + err.Error()}, nil
	}
	out, err := json.Marshal(card)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}

// UpdateCardStatus implements update_card_status(card_id, status): only the
// session currently holding the claim may move it (README task 9.5), and
// only into one of the three post-claim states — moving a card OUT of
// 'open' is claim_card's job alone, never this tool's.
type UpdateCardStatus struct {
	Resolver TeamResolver
	Updater  CardStatusUpdater
}

type updateCardStatusInput struct {
	CardID string `json:"card_id"`
	Status string `json:"status"`
}

var allowedCardStatusUpdates = map[string]bool{"in_progress": true, "done": true, "blocked": true}

func (UpdateCardStatus) ID() tools.ToolRef { return updateCardStatusRef }

func (UpdateCardStatus) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		ID:          updateCardStatusRef,
		Description: "Moves a card this session currently claims into in_progress, done, or blocked.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"card_id":{"type":"string"},"status":{"type":"string","enum":["in_progress","done","blocked"]}},"required":["card_id","status"]}`),
		EffectClass: tools.EffectClassMutating,
	}
}

func (UpdateCardStatus) Taint() tools.Taint { return boardTaint() }

func (UpdateCardStatus) IsConcurrencySafe(json.RawMessage) bool { return false }

func (u UpdateCardStatus) CheckPermissions(ctx context.Context, _ json.RawMessage, rc tools.RunContext) tools.PermissionResult {
	_, res := resolveTeam(ctx, u.Resolver, rc)
	return res
}

func (UpdateCardStatus) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	var req updateCardStatusInput
	if err := json.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if req.CardID == "" {
		return fmt.Errorf("card_id is required")
	}
	if _, err := uuid.Parse(req.CardID); err != nil {
		return fmt.Errorf("card_id is not a valid uuid: %w", err)
	}
	if !allowedCardStatusUpdates[req.Status] {
		return fmt.Errorf("status must be one of in_progress, done, blocked")
	}
	return nil
}

func (u UpdateCardStatus) Call(ctx context.Context, in json.RawMessage, rc tools.RunContext) (tools.Result, error) {
	var req updateCardStatusInput
	if err := json.Unmarshal(in, &req); err != nil {
		return tools.Result{}, err
	}
	cardID, err := uuid.Parse(req.CardID)
	if err != nil {
		return tools.Result{}, err
	}
	teamID, res := resolveTeam(ctx, u.Resolver, rc)
	if res.Decision == "deny" {
		return tools.Result{IsError: true, Reason: res.Reason}, nil
	}
	if u.Updater == nil {
		return tools.Result{IsError: true, Reason: "card status updater not wired"}, nil
	}
	card, ok, err := u.Updater.UpdateCardStatus(ctx, rc.TenantID, teamID, cardID, rc.SessionID, req.Status)
	if err != nil {
		return tools.Result{IsError: true, Reason: "update_card_status failed: " + err.Error()}, nil
	}
	if !ok {
		return tools.Result{IsError: true, Reason: "card_not_claimed_by_you: only the session holding the claim may update its status"}, nil
	}
	out, err := json.Marshal(card)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out}, nil
}
