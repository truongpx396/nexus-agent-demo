package main

import (
	"context"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/runs"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/skills"
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
