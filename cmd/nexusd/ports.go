package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/runs"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/retrieval"
	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// onSuspendFunc adapts oversight.Approvals.Create to the Postgres event log's
// suspend hook —
// rendering the ContextPackage a human approver sees (README task 5.6:
// "never a bare UUID") from the tool_use's own tool_id/effect_class/
// plaintext input, all of which kernel.SuspendRequest already carries
// (tools/pipeline.go's Ask branch is where EffectClass and CanonicalDigest
// both originate, in the one place that already has the descriptor).
func onSuspendFunc(approvals *oversight.Approvals) postgres.SuspendFunc {
	return func(ctx context.Context, req kernel.SuspendRequest) error {
		_, err := approvals.Create(ctx, oversight.CreateApprovalRequest{
			TenantID: req.TenantID, SessionID: req.SessionID, ToolUseEventID: req.ToolUseEventID,
			ToolID: req.ToolID, AskKind: req.AskKind, CanonicalDigest: req.CanonicalDigest,
			Context: oversight.ContextPackage{ToolID: req.ToolID, EffectClass: req.EffectClass, Input: req.Input},
		})
		return err
	}
}

// onDelegateFunc adapts delegate.Delegations.BindToolUse to the Postgres event
// log's delegate hook. kernel itself never sees internal/delegate.
func onDelegateFunc(delegations *delegate.Delegations) postgres.DelegateFunc {
	return func(ctx context.Context, req kernel.DelegateSuspendRequest) error {
		return delegations.BindToolUse(ctx, req.TenantID, req.ChildSessionID, req.ToolUseEventID)
	}
}

// nexusdDelegationSpawner adapts *internal/delegate.Delegations.Spawn to
// internal/tools/builtin.DelegationSpawner — the two packages deliberately
// carry independent SpawnRequest shapes (internal/tools/builtin/delegate.go's
// own doc comment on why), so this translation always lives at the wiring
// layer, never inside either package.
type nexusdDelegationSpawner struct {
	delegations *delegate.Delegations
}

func (s nexusdDelegationSpawner) Spawn(ctx context.Context, req builtin.SpawnRequest) (uuid.UUID, error) {
	return s.delegations.Spawn(ctx, delegate.SpawnRequest{
		TenantID: req.TenantID, ParentSessionID: req.ParentSessionID,
		AgentID: req.AgentID, Task: req.Task, ScopeGrant: req.ScopeGrant, ReturnSchema: req.ReturnSchema,
	})
}

// nexusdBoardAdapter adapts *internal/teams.Service to the four board
// tools' own structural interfaces (internal/tools/builtin/board.go) —
// translating between internal/teams.Card and builtin.BoardCard, the same
// deliberately-independent-shapes translation nexusdDelegationSpawner
// already performs for Delegate/Spawn. A thin, stateless value type (like
// nexusdDelegationSpawner), safe to construct fresh at each registration
// site above rather than threaded through as a shared field.
type nexusdBoardAdapter struct{ teams *teams.Service }

func toBoardCard(c teams.Card) builtin.BoardCard {
	var claimedBy string
	if c.ClaimedBySessionID != nil {
		claimedBy = c.ClaimedBySessionID.String()
	}
	return builtin.BoardCard{
		CardID: c.CardID.String(), Title: c.Title, Body: c.Body, Status: string(c.Status),
		Flagged: c.ScanStatus != teams.ScanClean, ClaimedBySessionID: claimedBy,
	}
}

func (a nexusdBoardAdapter) TeamIDFor(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, bool, error) {
	return a.teams.TeamIDFor(ctx, tenantID, sessionID)
}

func (a nexusdBoardAdapter) ReadBoard(ctx context.Context, tenantID, teamID, readerSessionID uuid.UUID) ([]builtin.BoardCard, error) {
	cards, err := a.teams.ReadBoard(ctx, tenantID, teamID, readerSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]builtin.BoardCard, len(cards))
	for i, c := range cards {
		out[i] = toBoardCard(c)
	}
	return out, nil
}

func (a nexusdBoardAdapter) ClaimCard(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID) (builtin.BoardCard, bool, error) {
	c, ok, err := a.teams.ClaimCard(ctx, tenantID, teamID, cardID, sessionID)
	if err != nil || !ok {
		return builtin.BoardCard{}, ok, err
	}
	return toBoardCard(c), true, nil
}

func (a nexusdBoardAdapter) WriteCard(ctx context.Context, req builtin.WriteCardRequest) (builtin.BoardCard, error) {
	c, err := a.teams.WriteCard(ctx, teams.WriteCardRequest{
		TenantID: req.TenantID, TeamID: req.TeamID, Title: req.Title, Body: req.Body, WrittenBySessionID: req.WrittenBySessionID,
	})
	if err != nil {
		return builtin.BoardCard{}, err
	}
	return toBoardCard(c), nil
}

func (a nexusdBoardAdapter) UpdateCardStatus(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID, status string) (builtin.BoardCard, bool, error) {
	c, ok, err := a.teams.UpdateCardStatus(ctx, tenantID, teamID, cardID, sessionID, teams.CardStatus(status))
	if err != nil || !ok {
		return builtin.BoardCard{}, ok, err
	}
	return toBoardCard(c), true, nil
}

// nexusdRetrieverAdapter adapts *internal/retrieval.Retriever to
// internal/tools/builtin.Searcher — translating retrieval.ScoredChunk into
// builtin.RetrievedChunk, the same deliberately-independent-shapes
// translation nexusdBoardAdapter and nexusdDelegationSpawner already
// perform for their own packages (each one's own doc comment explains why:
// internal/tools/builtin never imports the service package it's adapting).
type nexusdRetrieverAdapter struct{ retriever *retrieval.Retriever }

func (a nexusdRetrieverAdapter) Search(ctx context.Context, tenantID, sessionID uuid.UUID, query string, topK int) ([]builtin.RetrievedChunk, error) {
	results, err := a.retriever.Search(ctx, tenantID, sessionID, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]builtin.RetrievedChunk, len(results))
	for i, r := range results {
		out[i] = builtin.RetrievedChunk{
			DocID: r.DocID.String(), ChunkID: r.ChunkID.String(), ChunkIndex: r.ChunkIndex,
			Content: r.Content, Distance: r.Distance, SourceDigest: hex.EncodeToString(r.SourceDigest),
		}
	}
	return out, nil
}

// logSender is the demo's stand-in surfaces.Sender (README task 7.14) —
// logs the notification rather than actually reaching a human, the same
// honest-interim posture demoSafetyModel and the unsandboxed platform/shell
// fallback already take elsewhere in this file. A real Telegram/email
// Sender is Phase 11's; the point of this phase is the outbox's own
// durability discipline, not a new transport.
type logSender struct{}

func (logSender) Send(_ context.Context, surfaceID, recipient string, payload []byte) error {
	log.Info().Str("surface_id", surfaceID).Str("recipient", recipient).Str("payload", string(payload)).Msg("nexusd: outbox delivery (demo sender)")
	return nil
}

// nexusdSkillSetPort is the only implementation of runsrest.SkillSetPort this
// binary ships (README task 7.6) — resolves one tenant's admitted skill set
// (internal/config) against the process-wide trusted bundle set
// (newToolPipeline's admittedSkillBundles) into a SkillSet digest, at
// session-creation time. internal/runs/rest never imports internal/skills or
// internal/config directly, mirroring every other Port in this file.
type nexusdSkillSetPort struct {
	config  config.Loader
	bundles []skills.SkillBundle
}

func (p *nexusdSkillSetPort) Digest(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	cfg, err := p.config.Load(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load tenant config for skill set digest: %w", err)
	}
	return skills.BuildSkillSet(p.bundles, cfg.AdmittedSkillIDs).Digest, nil
}

// nexusdSessionLookup implements builtin.SessionUserLookup — the one place
// platform/connector_fetch re-derives WHO is calling from the durable session
// rather than trusting a client-claimed identity.
type nexusdSessionLookup struct{ reader runs.Reader }

func (l nexusdSessionLookup) UserIDForSession(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, error) {
	run, err := l.reader.GetRun(ctx, tenantID, sessionID)
	if err != nil {
		return uuid.Nil, err
	}
	return run.UserID, nil
}
