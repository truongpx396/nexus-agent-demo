package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/retrieval"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// chainReceiptFunc adapts internal/audit.Chain.Append to kernel.ReceiptFunc
// — the seam kernel/types.go declares locally so kernel itself never
// imports internal/audit (not on its own import allowlist).
func chainReceiptFunc(chain *audit.Chain) kernel.ReceiptFunc {
	return func(ctx context.Context, tx pgx.Tx, e store.Event) error {
		_, err := chain.Append(ctx, tx, e.TenantID, e.SessionID, e.Seq, e.EventID, string(e.Type), e.PayloadDigest)
		return err
	}
}

// onSuspendFunc adapts oversight.Approvals.Create to kernel.OnSuspend —
// rendering the ContextPackage a human approver sees (README task 5.6:
// "never a bare UUID") from the tool_use's own tool_id/effect_class/
// plaintext input, all of which kernel.SuspendRequest already carries
// (tools/pipeline.go's Ask branch is where EffectClass and CanonicalDigest
// both originate, in the one place that already has the descriptor).
func onSuspendFunc(approvals *oversight.Approvals) kernel.OnSuspend {
	return func(ctx context.Context, tx pgx.Tx, req kernel.SuspendRequest) error {
		_, err := approvals.Create(ctx, oversight.CreateApprovalRequest{
			TenantID: req.TenantID, SessionID: req.SessionID, ToolUseEventID: req.ToolUseEventID,
			ToolID: req.ToolID, AskKind: req.AskKind, CanonicalDigest: req.CanonicalDigest,
			Context: oversight.ContextPackage{ToolID: req.ToolID, EffectClass: req.EffectClass, Input: req.Input},
		})
		return err
	}
}

// onDelegateFunc adapts internal/delegate.Delegations.Bind to kernel.OnDelegate
// — the seam kernel/types.go declares locally so kernel itself never
// imports internal/delegate (not on its own import allowlist).
func onDelegateFunc(delegations *delegate.Delegations) kernel.OnDelegate {
	return func(ctx context.Context, tx pgx.Tx, req kernel.DelegateSuspendRequest) error {
		return delegations.Bind(ctx, tx, req.ChildSessionID, req.ToolUseEventID)
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

// nexusdSkillSetPort is the only implementation of rest.SkillSetPort this
// binary ships (README task 7.6) — resolves one tenant's admitted skill set
// (internal/config) against the process-wide trusted bundle set
// (newToolPipeline's admittedSkillBundles) into a SkillSet digest, at
// session-creation time. internal/surfaces/rest never imports
// internal/skills or internal/config directly, mirroring every other Port
// in this file.
type nexusdSkillSetPort struct {
	store   *store.Store
	bundles []skills.SkillBundle
}

func (p *nexusdSkillSetPort) Digest(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	cfg, err := config.LoadForTenant(ctx, p.store, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load tenant config for skill set digest: %w", err)
	}
	return skills.BuildSkillSet(p.bundles, cfg.AdmittedSkillIDs).Digest, nil
}

// nexusdRunCtlPort is the only implementation of rest.RunCtlPort this
// binary ships — the seam that lets internal/surfaces/rest drive
// internal/runctl (which itself imports kernel) without importing it
// directly, mirroring nexusdOversightPort's own role one struct down.
type nexusdRunCtlPort struct {
	ctl *runctl.Control
}

func (p *nexusdRunCtlPort) Cancel(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error {
	return p.ctl.Cancel(ctx, tenantID, sessionID, reason)
}

func (p *nexusdRunCtlPort) Steer(ctx context.Context, tenantID, sessionID uuid.UUID, input string) error {
	_, err := p.ctl.Steer(ctx, tenantID, sessionID, input)
	return err
}

// ResumeConversation adapts runctl.Control.ResumeConversation's
// iter.Seq2[store.Event, error] generator into the same <-chan rest.RunEvent
// shape kernelRunStarter.StartRun already returns — a run outlives the HTTP
// request that resumed it, so this drains the generator on its own goroutine
// exactly like StartRun's does, rather than blocking handleSteerRun on it.
func (p *nexusdRunCtlPort) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan rest.RunEvent, error) {
	events, err := p.ctl.ResumeConversation(ctx, tenantID, sessionID, input)
	if err != nil {
		return nil, err
	}
	ch := make(chan rest.RunEvent, 8)
	go func() {
		defer close(ch)
		for ev, err := range events {
			ch <- rest.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}

func (p *nexusdRunCtlPort) TightenAutonomy(ctx context.Context, tenantID, sessionID uuid.UUID, target string) error {
	return p.ctl.TightenAutonomy(ctx, tenantID, sessionID, target)
}

func (p *nexusdRunCtlPort) Fork(ctx context.Context, tenantID, sessionID uuid.UUID, atSeq int64, modelOverride string) (rest.ForkView, error) {
	result, err := p.ctl.Fork(ctx, tenantID, sessionID, atSeq, runctl.ForkOverrides{ModelID: modelOverride})
	if err != nil {
		return rest.ForkView{}, err
	}
	return rest.ForkView{
		SessionID: result.SessionID.String(), DigestDiverged: result.DigestDiverged,
		ParentDigest: hex.EncodeToString(result.ParentDigest), ChildDigest: hex.EncodeToString(result.ChildDigest),
	}, nil
}

// nexusdOversightPort is the only implementation of rest.OversightPort this
// binary ships — the seam that lets internal/surfaces/rest drive
// internal/oversight (which itself imports kernel for Kernel.Resume)
// without importing it directly, exactly mirroring kernelRunStarter's own
// role for kernel.Kernel.Run.
type nexusdOversightPort struct {
	approvals   *oversight.Approvals
	resumer     *oversight.Resumer
	delegations *delegate.Delegations
	teams       *teams.Service
}

func (p *nexusdOversightPort) GetApproval(ctx context.Context, tenantID, approvalID uuid.UUID) (rest.ApprovalView, error) {
	ap, err := p.approvals.Get(ctx, tenantID, approvalID)
	if err != nil {
		return rest.ApprovalView{}, err
	}
	return toApprovalView(ap), nil
}

func (p *nexusdOversightPort) ListPendingApprovals(ctx context.Context, tenantID uuid.UUID) ([]rest.ApprovalView, error) {
	aps, err := p.approvals.ListPending(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	views := make([]rest.ApprovalView, len(aps))
	for i, ap := range aps {
		views[i] = toApprovalView(ap)
	}
	return views, nil
}

func toApprovalView(ap oversight.Approval) rest.ApprovalView {
	ctxJSON, _ := json.Marshal(ap.Context) //nolint:errcheck // ContextPackage is always marshalable (json.RawMessage + strings)
	return rest.ApprovalView{
		ApprovalID: ap.ApprovalID.String(), SessionID: ap.SessionID.String(), ToolID: ap.ToolID,
		AskKind: ap.AskKind, Status: string(ap.Status), Context: ctxJSON, ExpiresAt: ap.ExpiresAt, CreatedAt: ap.CreatedAt,
	}
}

func (p *nexusdOversightPort) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) rest.ResumeOutcome {
	out := drainResume(p.resumer.Grant(ctx, tenantID, approvalID, decidedBy))
	p.onResumed(ctx, tenantID, out)
	return out
}

func (p *nexusdOversightPort) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) rest.ResumeOutcome {
	out := drainResume(p.resumer.GrantModified(ctx, tenantID, approvalID, decidedBy, modifiedInput))
	p.onResumed(ctx, tenantID, out)
	return out
}

func (p *nexusdOversightPort) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) rest.ResumeOutcome {
	out := drainResume(p.resumer.Deny(ctx, tenantID, approvalID, decidedBy, reason))
	p.onResumed(ctx, tenantID, out)
	return out
}

// onResumed is README task 8.10's other resume-path hook: a session an
// ordinary tool approval just resumed may ALSO be a delegation's child —
// OnChildTerminal is a documented no-op unless it is (the overwhelming
// majority of approval decisions resolve an ordinary root run).
func (p *nexusdOversightPort) onResumed(ctx context.Context, tenantID uuid.UUID, out rest.ResumeOutcome) {
	if out.Err != "" || out.SessionID == "" {
		return
	}
	sessionID, err := uuid.Parse(out.SessionID)
	if err != nil {
		return
	}
	if p.delegations != nil {
		if err := p.delegations.OnChildTerminal(ctx, tenantID, sessionID); err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("nexusd: resolve delegation after approval-resumed child failed")
		}
	}
	// An approval-resumed session may ALSO be a team member (README task
	// 9.9) — OnMemberTerminal is a documented no-op unless it is, mirroring
	// OnChildTerminal's own call just above.
	if p.teams != nil {
		if err := p.teams.OnMemberTerminal(ctx, tenantID, sessionID); err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("nexusd: resolve team after approval-resumed member failed")
		}
	}
}

// drainResume fully consumes a Kernel.Resume generator — the same
// drain-into-a-summary kernelRunStarter's own goroutine does for
// Kernel.Run, except synchronous: an approval decision's HTTP response
// waits for the resumed run to finish (or suspend again), rather than
// handing back a channel the way a fresh run's 202 does. See rest.
// ResumeOutcome's own doc comment for why that's an acceptable trade for
// this endpoint.
func drainResume(events iter.Seq2[store.Event, error]) rest.ResumeOutcome {
	var out rest.ResumeOutcome
	for ev, err := range events {
		if err != nil {
			out.Err = err.Error()
			return out
		}
		out.SessionID = ev.SessionID.String()
		out.EventsAppended++
	}
	return out
}

// nexusdSessionLookup implements builtin.SessionUserLookup — the one place
// platform/connector_fetch re-derives WHO is calling from the durable
// session row rather than trusting a client-claimed identity.
type nexusdSessionLookup struct{ store *store.Store }

func (l nexusdSessionLookup) UserIDForSession(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, error) {
	var userID uuid.UUID
	err := l.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		userID = sess.UserID
		return nil
	})
	return userID, err
}
