package teams

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

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
			log.Error().Err(err).Any("team_id", teamID).Any("agent_id", m.AgentID).Msg("teams: spawn member failed")
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
				log.Error().Err(err).Any("member_session_id", memberID).Msg("teams: member run errored")
				return
			}
		}
		if err := s.OnMemberTerminal(bg, tenantID, memberID); err != nil {
			log.Error().Err(err).Any("member_session_id", memberID).Msg("teams: resolve after member run failed")
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
			log.Error().Err(err).Any("team_id", teamID).Any("member_session_id", m).Msg("teams: reap member failed")
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
			log.Error().Err(err).Any("team_id", id).Msg("teams: backstop reap failed")
		}
	}
	return nil
}
