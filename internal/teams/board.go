package teams

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TeamIDFor resolves sessionID's own team_id — the ONLY input the four
// board tools' CheckPermissions/Call ever trust to decide which team a
// session may touch (never taken from a tool's own input; see
// internal/tools/builtin/board.go's own doc comment).
func (s *Service) TeamIDFor(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, bool, error) {
	var id uuid.UUID
	var ok bool
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, ok, err = teamIDForSession(ctx, tx, sessionID)
		return err
	})
	return id, ok, err
}

// ReadBoard lists every card on teamID's board. A flagged card's body is
// redacted and its taint is never folded into the reader (task 9.7: never
// surfaced to another peer's context — there is nothing clean to launder
// from a card nobody is shown); every clean card's body is returned AND
// folds its taint_state into readerSessionID's own running state (task
// 9.6), both in-process (TaintFolder.FoldTaint) and durably
// (EventTaintTransition on the reader's own log).
func (s *Service) ReadBoard(ctx context.Context, tenantID, teamID, readerSessionID uuid.UUID) ([]Card, error) {
	var cards []Card
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cards, err = listCards(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := make([]Card, 0, len(cards))
	for _, c := range cards {
		if c.ScanStatus != ScanClean {
			c.Body = ""
			out = append(out, c)
			continue
		}
		if err := s.foldCardRead(ctx, tenantID, readerSessionID, c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ClaimCard atomically claims one open, clean card (task 9.4's own SKIP
// LOCKED claim — see store.go's claimCard) and, on success, folds its
// taint into the claimant exactly like ReadBoard does (claiming is a read
// too: the claimant needs the body to act on it).
func (s *Service) ClaimCard(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID) (Card, bool, error) {
	var card Card
	var claimed bool
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		card, claimed, err = claimCard(ctx, tx, tenantID, teamID, cardID, sessionID)
		return err
	})
	if err != nil || !claimed {
		return Card{}, false, err
	}
	if err := s.foldCardRead(ctx, tenantID, sessionID, card); err != nil {
		return Card{}, false, err
	}
	return card, true, nil
}

func (s *Service) foldCardRead(ctx context.Context, tenantID, readerSessionID uuid.UUID, c Card) error {
	if s.cfg.Pipeline != nil {
		s.cfg.Pipeline.FoldTaint(readerSessionID, c.TaintState)
	}
	return s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.appendEvent(ctx, tx, tenantID, readerSessionID, store.EventTaintTransition, nil, nil, cardReadTaintTransitionPayload{
			CardID: c.CardID, Engaged: c.TaintState,
		})
		return err
	})
}

// WriteCardRequest is WriteCard's own input — deliberately primitive-typed
// (no Card, no internal state) so a caller (internal/tools/builtin.WriteCard)
// never needs to construct anything but plain values.
type WriteCardRequest struct {
	TenantID           uuid.UUID
	TeamID             uuid.UUID
	Title              string
	Body               string
	WrittenBySessionID uuid.UUID
}

// WriteCard scans body through the SAME injection/exfiltration scanner
// internal/memory already uses for a memory file (task 9.7, reusing task
// 7.1) before ever setting injection_scan_status to clean, and captures
// WrittenBySessionID's own current taint state at this exact moment (task
// 9.3's copy-at-write). A flagged card is inserted (never silently dropped
// — the audit trail needs the row to exist) but EventBoardCardFlagged
// makes the fail-closed decision visible, mirroring EventSkillCapabilityIgnored's
// own role for skills.
func (s *Service) WriteCard(ctx context.Context, req WriteCardRequest) (Card, error) {
	var taint [3]bool
	if s.cfg.Pipeline != nil {
		taint = s.cfg.Pipeline.TaintStateFor(req.WrittenBySessionID)
	}

	scanStatus, findings := memory.Screen(req.Body)
	cardScan := ScanClean
	var findingStrs []string
	if scanStatus != memory.StatusClean {
		cardScan = ScanFlagged
		findingStrs = findings
	}

	var card Card
	err := s.Store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		card, err = insertCard(ctx, tx, req.TenantID, req.TeamID, req.Title, req.Body, req.WrittenBySessionID, taint, cardScan, findingStrs)
		if err != nil {
			return err
		}
		if cardScan == ScanFlagged {
			_, err = s.appendEvent(ctx, tx, req.TenantID, req.WrittenBySessionID, store.EventBoardCardFlagged, nil, nil, boardCardFlaggedPayload{
				CardID: card.CardID, TeamID: req.TeamID, Findings: findingStrs,
			})
		}
		return err
	})
	if err != nil {
		return Card{}, err
	}
	if cardScan == ScanFlagged {
		card.Body = "" // never surfaced, not even echoed back in the writer's own tool_result
	}
	return card, nil
}

// UpdateCardStatus moves cardID into status — only the session that
// currently holds the claim may do so (store.go's updateCardStatus own
// WHERE clause), and ok=false (not an error) is fail-closed's honest
// answer for "not claimed by you" exactly like ClaimCard's own claimed=false.
func (s *Service) UpdateCardStatus(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID, status CardStatus) (Card, bool, error) {
	var card Card
	var ok bool
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		card, ok, err = updateCardStatus(ctx, tx, teamID, cardID, sessionID, status)
		return err
	})
	return card, ok, err
}

// Get loads one team by id — nexusctl/REST/test fixtures' own lookup;
// ordinary board/lifecycle code never needs this (it works off the ids it
// already has).
func (s *Service) Get(ctx context.Context, tenantID, teamID uuid.UUID) (Team, error) {
	var out Team
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = getTeam(ctx, tx, teamID)
		return err
	})
	return out, err
}
