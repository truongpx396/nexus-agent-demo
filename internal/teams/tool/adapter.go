package tool

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/teams"
	"github.com/truongpx396/nexus-agent/internal/tools"
)

// Tools returns the four board tools over svc, in registration order.
func Tools(svc *teams.Service) []tools.Tool {
	b := boardAdapter{teams: svc}
	return []tools.Tool{
		ReadBoard{Resolver: b, Reader: b},
		ClaimCard{Resolver: b, Claimer: b},
		WriteCard{Resolver: b, Writer: b},
		UpdateCardStatus{Resolver: b, Updater: b},
	}
}

// boardAdapter presents a *teams.Service to the tools as their own primitive-typed
// view: it translates teams.Card to BoardCard, which is where a flagged card's
// body stays out of what the model sees. It is stateless, so Tools builds one
// and hands the same value to every tool.
type boardAdapter struct{ teams *teams.Service }

func toBoardCard(c teams.Card) BoardCard {
	var claimedBy string
	if c.ClaimedBySessionID != nil {
		claimedBy = c.ClaimedBySessionID.String()
	}
	return BoardCard{
		CardID: c.CardID.String(), Title: c.Title, Body: c.Body, Status: string(c.Status),
		Flagged: c.ScanStatus != teams.ScanClean, ClaimedBySessionID: claimedBy,
	}
}

func (a boardAdapter) TeamIDFor(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, bool, error) {
	return a.teams.TeamIDFor(ctx, tenantID, sessionID)
}

func (a boardAdapter) ReadBoard(ctx context.Context, tenantID, teamID, readerSessionID uuid.UUID) ([]BoardCard, error) {
	cards, err := a.teams.ReadBoard(ctx, tenantID, teamID, readerSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]BoardCard, len(cards))
	for i, c := range cards {
		out[i] = toBoardCard(c)
	}
	return out, nil
}

func (a boardAdapter) ClaimCard(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID) (BoardCard, bool, error) {
	c, ok, err := a.teams.ClaimCard(ctx, tenantID, teamID, cardID, sessionID)
	if err != nil || !ok {
		return BoardCard{}, ok, err
	}
	return toBoardCard(c), true, nil
}

func (a boardAdapter) WriteCard(ctx context.Context, req WriteCardRequest) (BoardCard, error) {
	c, err := a.teams.WriteCard(ctx, teams.WriteCardRequest{
		TenantID: req.TenantID, TeamID: req.TeamID, Title: req.Title, Body: req.Body, WrittenBySessionID: req.WrittenBySessionID,
	})
	if err != nil {
		return BoardCard{}, err
	}
	return toBoardCard(c), nil
}

func (a boardAdapter) UpdateCardStatus(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID, status string) (BoardCard, bool, error) {
	c, ok, err := a.teams.UpdateCardStatus(ctx, tenantID, teamID, cardID, sessionID, teams.CardStatus(status))
	if err != nil || !ok {
		return BoardCard{}, ok, err
	}
	return toBoardCard(c), true, nil
}
