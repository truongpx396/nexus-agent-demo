package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// skillActivatedPayload/skillCapabilityIgnoredPayload are what
// EventSkillActivated/EventSkillCapabilityIgnored carry — deliberately
// minimal, mirroring claimedPayload/claimResolvedPayload's own style
// (claims.go): never the skill's body (that's the tool_result the ordinary
// pipeline already logs), just the audit-relevant facts.
type skillActivatedPayload struct {
	SkillID     string   `json:"skill_id"`
	HeldToolIDs []string `json:"held_tool_ids"`
}

type skillCapabilityIgnoredPayload struct {
	SkillID string `json:"skill_id"`
	ToolID  string `json:"tool_id"`
}

// SkillEventRecorder implements builtin.SkillEvents (README task 7.8)
// against durable storage, mirroring ClaimTracker's own role one struct
// over: internal/tools/builtin.ActivateSkill declares the interface it
// needs, this package supplies the real implementation (store append +
// audit-chain append via deps.appendEvent), and cmd/nexusd wires it in.
// Neither internal/tools nor kernel ever import this package or learn
// skills exist.
type SkillEventRecorder struct {
	deps
}

// NewSkillEventRecorder takes its collaborators directly, exactly like
// NewClaimTracker — cmd/nexusd wires this into
// internal/tools/builtin.ActivateSkill before a *runctl.Control can exist
// (the same ordering constraint NewClaimTracker's own doc comment names).
func NewSkillEventRecorder(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *SkillEventRecorder {
	return &SkillEventRecorder{deps: deps{Store: st, Keys: keys, Chain: chain}}
}

// Activated implements builtin.SkillEvents.Activated.
func (r *SkillEventRecorder) Activated(ctx context.Context, tenantID, sessionID uuid.UUID, skillID string, heldToolIDs []string) error {
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := r.appendEvent(ctx, tx, tenantID, sessionID, store.EventSkillActivated, &skillID, nil,
			skillActivatedPayload{SkillID: skillID, HeldToolIDs: heldToolIDs})
		return err
	})
	if err != nil {
		return fmt.Errorf("runctl: record skill_activated for %s: %w", skillID, err)
	}
	return nil
}

// CapabilityIgnored implements builtin.SkillEvents.CapabilityIgnored.
func (r *SkillEventRecorder) CapabilityIgnored(ctx context.Context, tenantID, sessionID uuid.UUID, skillID, toolID string) error {
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := r.appendEvent(ctx, tx, tenantID, sessionID, store.EventSkillCapabilityIgnored, &toolID, nil,
			skillCapabilityIgnoredPayload{SkillID: skillID, ToolID: toolID})
		return err
	})
	if err != nil {
		return fmt.Errorf("runctl: record skill_capability_ignored for %s/%s: %w", skillID, toolID, err)
	}
	return nil
}
