package teams

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// TaintFolder is the two operations this package needs from
// internal/tools.Pipeline — declared here rather than depending on
// *tools.Pipeline directly, mirroring internal/delegate's own TaintFolder
// (README task 8.11's own granularity idiom, reused verbatim for task 9.6's
// read-time fold instead of a return-time one). TaintStateFor captures a
// writer's own engaged legs at write time (copy-at-write, board_cards'
// own taint_state column); FoldTaint folds a card's stored taint_state into
// a reader's own running state at read time.
type TaintFolder interface {
	TaintStateFor(sessionID uuid.UUID) [3]bool
	FoldTaint(sessionID uuid.UUID, engaged [3]bool)
}

// Canceler is the session-terminating primitive endTeam reaps still-active
// members with (README task 9.9, reusing 8.14's own reaping discipline) —
// satisfied structurally by *internal/runctl.Control.Cancel, "the sole
// producer of aborted" (that method's own doc comment).
type Canceler interface {
	Cancel(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error
}

// Config is Service's construction-time collaborators beyond deps — mirrors
// internal/delegate.Config's own shape and its own reason: no per-tenant/
// per-agent config store exists yet, so one process-wide system prompt and
// resident catalog cover a team member exactly like they cover a root run.
type Config struct {
	Kernel      *kernel.Kernel
	Pipeline    TaintFolder
	Canceler    Canceler
	System      string
	Catalog     []provider.ToolSchema
	LoadedTools []string
	MaxTurns    int
}

// Service is the team transaction (README tasks 9.1-9.9): creating a fixed
// roster, tracking its shared board and budget envelope, and ending the
// team — construct once, share across the process, the same convention
// every other transactional component in this codebase
// (oversight.Approvals, delegate.Delegations, cost.Gate, ...) follows.
type Service struct {
	deps
	cfg Config
}

func NewService(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *Service {
	return &Service{deps: deps{Store: st, Keys: keys, Chain: chain}}
}

// Wire attaches Service's runtime collaborators — split from NewService the
// same way internal/delegate.Delegations.Wire is, so a test can construct a
// bare *Service against only deps (store_test.go-style fixtures) without
// also standing up a Kernel.
func (s *Service) Wire(cfg Config) *Service {
	s.cfg = cfg
	return s
}

// CreateTeamRequest is everything CreateTeam needs: the fixed roster, an
// optional set of seed cards, and the whole-team ceiling the caller has
// already sized for the roster's worst case (README task 9.8 — sizing that
// judgment is the caller's, the same way a delegate_fanout plan step's own
// config names its own ceiling before internal/delegate.CreateEnvelope ever
// runs).
type CreateTeamRequest struct {
	TenantID         uuid.UUID
	CreatorSessionID uuid.UUID
	Name             string
	Members          []MemberSpec
	Cards            []CardSpec
	Ceiling          cost.Money
}

// CreateTeam reserves the shared envelope, creates the team and its seed
// cards, and starts every roster member's own kernel.Run in the background
// — CreateTeam itself never blocks on any member's completion; each
// member's own goroutine calls OnMemberTerminal when it finishes.
//
// The depth/leaf check (README task 9.10: "a team member is a leaf... no
// recursive teams") runs here, in the same place and against the same
// MaxDepth bound internal/tools/builtin/delegate.go's own CheckPermissions
// re-derives for platform/delegate — a session already at MaxDepth cannot
// create a team any more than it can delegate further, and a session
// created AS a team member is always already at that bound (spawnMember
// pins Depth = creator.Depth + 1, never less), so a team member's own
// attempt to call CreateTeam again is refused by the exact same inequality,
// not a separate "is this a team member" flag that a future call site could
// forget to check.
func (s *Service) CreateTeam(ctx context.Context, req CreateTeamRequest) (uuid.UUID, error) {
	if s.cfg.Kernel == nil {
		return uuid.Nil, fmt.Errorf("teams: CreateTeam called before Wire")
	}
	if len(req.Members) == 0 {
		return uuid.Nil, fmt.Errorf("teams: roster must name at least one member")
	}

	var creator store.Session
	teamID := uuid.New()
	var envelopeID uuid.UUID
	err := s.Store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		creator, err = store.GetSession(ctx, tx, req.CreatorSessionID)
		if err != nil {
			return err
		}
		if creator.Depth+1 > MaxDepth {
			return fmt.Errorf("teams: session %s may not create a team (depth bound exceeded — task 9.10)", req.CreatorSessionID)
		}

		if err := insertTeam(ctx, tx, teamID, req.TenantID, req.Name, req.CreatorSessionID, req.Members); err != nil {
			return err
		}

		envelopeID, err = createEnvelope(ctx, tx, req.TenantID, teamID, req.Ceiling, len(req.Members))
		if err != nil {
			return err
		}
		if err := setTeamEnvelope(ctx, tx, teamID, envelopeID); err != nil {
			return err
		}

		// Seed cards are written under the CREATOR's own current taint
		// state — the same copy-at-write rule WriteCard enforces for every
		// card written after the team exists (task 9.3), never scanned
		// (this package's own CardSpec doc comment on why first-party seed
		// content sits in a different trust position than a peer's write).
		var creatorEngaged [3]bool
		if s.cfg.Pipeline != nil {
			creatorEngaged = s.cfg.Pipeline.TaintStateFor(req.CreatorSessionID)
		}
		for _, c := range req.Cards {
			if _, err := insertCard(ctx, tx, req.TenantID, teamID, c.Title, c.Body, req.CreatorSessionID, creatorEngaged, ScanClean, nil); err != nil {
				return err
			}
		}

		agentIDs := make([]string, len(req.Members))
		for i, m := range req.Members {
			agentIDs[i] = m.AgentID
		}
		_, err = s.appendEvent(ctx, tx, req.TenantID, req.CreatorSessionID, store.EventTeamCreated, nil, nil, teamCreatedPayload{
			TeamID: teamID, Name: req.Name, Roster: agentIDs, CardCount: len(req.Cards),
		})
		return err
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("teams: create: %w", err)
	}

	for _, m := range req.Members {
		if err := s.spawnMember(context.Background(), req.TenantID, teamID, envelopeID, creator, m); err != nil {
			slog.Error("teams: spawn member failed", "team_id", teamID, "agent_id", m.AgentID, "error", err)
		}
	}
	return teamID, nil
}

// spawnMember creates one ordinary member session (task 9.2: an ordinary
// session, delegation_role="team_member", team_id set) and starts its own
// kernel.Run in the background under a CLONED Kernel whose Budget draws
// from the team's shared envelope instead of the process's real
// internal/cost.Gate (task 9.8) — every other field (Provider/Tools/Store/
// Receipts/OnSuspend/...) stays identical, the same purely-budget-routing
// clone internal/delegate.Delegations.Spawn already performs for a
// delegate_fanout child.
func (s *Service) spawnMember(ctx context.Context, tenantID, teamID, envelopeID uuid.UUID, creator store.Session, member MemberSpec) error {
	memberID := uuid.New()
	var dek crypto.DEK
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dek, err = s.Keys.NewDEK(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		tid := teamID
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: memberID, SessionKey: memberID.String(), TenantID: tenantID,
			SurfaceID: "team", UserID: creator.UserID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: creator.HarnessDigest, DataLabel: creator.DataLabel,
			RouteModelID: creator.RouteModelID, RouteReason: creator.RouteReason,
			AutonomyLevel: creator.AutonomyLevel, RootSessionID: creator.RootSessionID,
			Depth: creator.Depth + 1, DelegationRole: "team_member", TeamID: &tid,
		})
	})
	if err != nil {
		return fmt.Errorf("teams: create member session: %w", err)
	}

	perCall, currency, err := envelopePerCallEstimate(ctx, s.Store, tenantID, envelopeID)
	if err != nil {
		return fmt.Errorf("teams: size member reservation: %w", err)
	}
	clone := *s.cfg.Kernel
	clone.Budget = &EnvelopeBudgetGate{Store: s.Store, EnvelopeID: envelopeID, TenantID: tenantID, PerCallEstimate: perCall, Currency: currency}

	memberState := &kernel.RunState{TenantID: tenantID, SessionID: memberID, Seal: sealFuncFor(dek, tenantID, memberID)}
	memberCfg := kernel.RunConfig{
		System: s.cfg.System, Catalog: s.cfg.Catalog, LoadedTools: s.cfg.LoadedTools,
		ModelID: creator.RouteModelID, MaxTurns: s.cfg.MaxTurns,
		Input: member.Task, AutonomyLevel: creator.AutonomyLevel,
	}

	go func() {
		bg := context.Background()
		for _, err := range clone.Run(bg, memberState, memberCfg) {
			if err != nil {
				slog.Error("teams: member run errored", "member_session_id", memberID, "error", err)
				return
			}
		}
		if err := s.OnMemberTerminal(bg, tenantID, memberID); err != nil {
			slog.Error("teams: resolve after member run failed", "member_session_id", memberID, "error", err)
		}
	}()
	return nil
}

func sealFuncFor(dek crypto.DEK, tenantID, sessionID uuid.UUID) kernel.SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("teams: seal member event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}

// OnMemberTerminal is the one entry point every place in this codebase that
// might just have driven a team member session to completion should call,
// unconditionally — mirrors internal/delegate.Delegations.OnChildTerminal's
// own role and call sites exactly (Spawn's own goroutine above, cmd/
// nexusd's queue runner after a crash-recovered resume, and the approval
// grant/deny handlers after an approval-suspended member resumes). A
// documented no-op for every non-team-member session (the overwhelming
// majority) and for a team member that hasn't reached a terminal status yet
// (a later call site will find it).
func (s *Service) OnMemberTerminal(ctx context.Context, tenantID, memberSessionID uuid.UUID) error {
	var member store.Session
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		member, err = store.GetSession(ctx, tx, memberSessionID)
		return err
	})
	if err != nil {
		return err
	}
	if member.TeamID == nil {
		return nil
	}
	if member.Status != store.SessionStatusCompleted && member.Status != store.SessionStatusFailed {
		return nil
	}

	// The envelope exhausting is an immediate, explicit trigger (task 9.9),
	// not something left to arrive eventually as every other member's OWN
	// next Reserve call also starts failing: this member's own terminal
	// reason already told us the shared ceiling is at zero.
	if member.TerminalReason != nil && *member.TerminalReason == string(kernel.ReasonCostExhausted) {
		return s.endTeam(ctx, tenantID, *member.TeamID, StatusCeilingExhausted,
			fmt.Sprintf("member session %s exhausted the shared team budget envelope", memberSessionID))
	}
	return s.maybeComplete(ctx, tenantID, *member.TeamID)
}

// maybeComplete is task 9.9's own natural-completion predicate: the board
// has no open/claimed cards left AND every member has reached a terminal
// status. If members finish while cards remain open, the team is left
// active rather than guessed at — SweepBackstop's own wall-clock trigger is
// what eventually ends a team stuck in that state, deliberately never a
// special case bolted onto this method.
func (s *Service) maybeComplete(ctx context.Context, tenantID, teamID uuid.UUID) error {
	var openOrClaimed, activeMembers int
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		openOrClaimed, activeMembers, err = boardAndMemberCounts(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return err
	}
	if openOrClaimed == 0 && activeMembers == 0 {
		return s.endTeam(ctx, tenantID, teamID, StatusCompleted, "board emptied and every member reached a terminal state")
	}
	return nil
}

// endTeam is the only path any of Complete/CeilingExhausted/Aborted ever
// takes (README task 9.9): atomically claim the transition out of active
// (endTeamIfActive — a second, concurrent caller for the same team is a
// documented no-op, not an error), reap every still-active member exactly
// like a delegation parent reaps its still-open children (task 8.14), then
// append ONE EventTeamEnded onto the coordinator's own log.
func (s *Service) endTeam(ctx context.Context, tenantID, teamID uuid.UUID, status Status, reason string) error {
	var coordinatorSessionID uuid.UUID
	var claimed bool
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		coordinatorSessionID, claimed, err = endTeamIfActive(ctx, tx, teamID, status, reason)
		return err
	})
	if err != nil || !claimed {
		return err
	}

	var memberIDs []uuid.UUID
	err = s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		memberIDs, err = listActiveMemberSessionIDs(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return fmt.Errorf("teams: list active members to reap for team %s: %w", teamID, err)
	}
	for _, m := range memberIDs {
		if s.cfg.Canceler == nil {
			continue
		}
		if err := s.cfg.Canceler.Cancel(ctx, tenantID, m, fmt.Sprintf("team %s: %s", status, reason)); err != nil {
			slog.Error("teams: reap member failed", "team_id", teamID, "member_session_id", m, "error", err)
		}
	}

	return s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.appendEvent(ctx, tx, tenantID, coordinatorSessionID, store.EventTeamEnded, nil, nil, teamEndedPayload{
			TeamID: teamID, Status: string(status), Reason: reason,
		})
		return err
	})
}

// SweepBackstop is task 9.9's own wall-clock trigger: every 'active' team in
// tenantID older than backstop is ended as 'aborted' — a periodic caller
// (cmd/nexusd's own startTeamBackstopLoop, mirroring startAnchorLoop) is
// what actually schedules this; SweepBackstop itself is a single pass.
func (s *Service) SweepBackstop(ctx context.Context, tenantID uuid.UUID, backstop time.Duration) error {
	var stale []uuid.UUID
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stale, err = listStaleActiveTeamIDs(ctx, tx, tenantID, time.Now().Add(-backstop))
		return err
	})
	if err != nil {
		return fmt.Errorf("teams: sweep backstop: %w", err)
	}
	for _, id := range stale {
		if err := s.endTeam(ctx, tenantID, id, StatusAborted, "wall-clock backstop exceeded"); err != nil {
			slog.Error("teams: backstop reap failed", "team_id", id, "error", err)
		}
	}
	return nil
}

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
