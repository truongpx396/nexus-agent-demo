package runs

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Launcher is the use case a surface calls to begin a brand-new run: admit a
// session, mint its data key, and hand the run to a Starter. It is the one
// definition of "what starting a run means" — REST, Telegram, Zalo and email
// used to each carry their own copy of the sequence.
type Launcher interface {
	Launch(ctx context.Context, req LaunchRequest) (Launched, error)
}

// LaunchRequest is what a surface knows when it starts a run: who is asking,
// through which surface, and the facts of the request itself. Everything that
// can be defaulted or derived — routing, the harness digest, the data key —
// is the Service's job, not the surface's.
type LaunchRequest struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	// SurfaceID names the surface ("rest", "telegram", ...), recorded on the
	// session.
	SurfaceID string
	// SessionKey is the surface's stable handle for the conversation, so a
	// later message can find this session again. Empty: the session id.
	SessionKey string
	Input      string

	// DataLabel and Difficulty drive model routing. Empty means
	// provider.DataLabelInternal and provider.DifficultySimple.
	DataLabel  provider.DataLabel
	Difficulty provider.Difficulty
	// Autonomy pins the session's autonomy level. Empty means "supervised".
	Autonomy string
	// BudgetUSD, if set, is a per-session hard ceiling in decimal USD.
	BudgetUSD string
	// Conversational opts the session into pause-not-terminate semantics.
	Conversational bool

	// SkillSetDigest and MCPCatalogDigest are folded into the session's
	// harness digest alongside the process-wide catalog digest; MCPCatalog is
	// the matching per-user tool catalog the run adds to the base one.
	SkillSetDigest   []byte
	MCPCatalogDigest []byte
	MCPCatalog       []provider.ToolSchema
}

// Launched is a run that has been accepted. Events carries every event the
// run produces, in order, until it ends.
type Launched struct {
	SessionID uuid.UUID
	Events    <-chan Event
}

// NotAdmittedError reports a request the control plane refused as invalid (a
// bad autonomy level, an unparseable budget). It is the caller's problem, not
// a system failure: a surface should answer it as a bad request.
type NotAdmittedError struct{ Reason string }

func (e *NotAdmittedError) Error() string { return "run not admitted: " + e.Reason }

// Admitter is the part of the control plane Launch needs: durably admitting a
// session (and its optional session-scoped budget). controlplane.Port
// satisfies it.
type Admitter interface {
	AdmitRun(ctx context.Context, req controlplane.AdmitRunV1) (controlplane.AdmitRunResultV1, error)
}

// KeyProvider mints the data key a new session's events are sealed under.
// Encryption stays on the data-plane side of the control-plane boundary, so
// it is deliberately not part of Admitter.
type KeyProvider interface {
	NewDEK(ctx context.Context, tenantID uuid.UUID) (crypto.DEK, error)
}

// Service implements Launcher over its three ports.
type Service struct {
	Admit   Admitter
	Keys    KeyProvider
	Starter Starter

	// CatalogManifestDigest is folded into every new session's harness
	// digest, so a session records exactly which tool catalog it ran under.
	CatalogManifestDigest []byte
}

var _ Launcher = (*Service)(nil)

// Launch admits a session, mints its key, and starts the run. A failure after
// admission leaves an admitted session with no events — bounded, inert state
// (controlplane.Port's own doc comment), never a partial effect.
func (s *Service) Launch(ctx context.Context, req LaunchRequest) (Launched, error) {
	if req.DataLabel == "" {
		req.DataLabel = provider.DataLabelInternal
	}
	if req.Difficulty == "" {
		req.Difficulty = provider.DifficultySimple
	}
	if req.Autonomy == "" {
		req.Autonomy = "supervised"
	}
	route := provider.Route(req.DataLabel, req.Difficulty)

	sessionID := uuid.New()
	digest := harness.Digest(harness.Config{
		SystemPromptVersion:   "phase2-v1",
		CatalogManifestDigest: s.CatalogManifestDigest,
		SkillSetDigest:        req.SkillSetDigest,
		PromptMode:            "phase2-single-shot",
		MCPCatalogDigest:      req.MCPCatalogDigest,
	})

	admitted, err := s.Admit.AdmitRun(ctx, controlplane.AdmitRunV1{
		TenantID: req.TenantID, UserID: req.UserID, SessionID: sessionID,
		SessionKey: req.SessionKey, SurfaceID: req.SurfaceID,
		DataLabel: string(req.DataLabel), RouteModelID: route.ModelID, RouteReason: route.Reason,
		Autonomy: req.Autonomy, BudgetUSD: req.BudgetUSD, HarnessDigest: digest,
		Conversational: req.Conversational,
	})
	if err != nil {
		return Launched{}, fmt.Errorf("admit run: %w", err)
	}
	if !admitted.Admitted {
		return Launched{}, &NotAdmittedError{Reason: admitted.Reason}
	}

	dek, err := s.Keys.NewDEK(ctx, req.TenantID)
	if err != nil {
		return Launched{}, fmt.Errorf("provision session key: %w", err)
	}

	loaded := make([]string, len(req.MCPCatalog))
	for i, c := range req.MCPCatalog {
		loaded[i] = c.Name
	}
	// A run outlives the request that started it, so it keeps the caller's
	// context values (trace ids) but not its cancellation.
	events, err := s.Starter.StartRun(context.WithoutCancel(ctx), Request{
		SessionID: sessionID, TenantID: req.TenantID,
		Seal:  SealFor(dek, req.TenantID, sessionID),
		Input: req.Input, ModelID: route.ModelID, AutonomyLevel: req.Autonomy,
		ExtraCatalog: req.MCPCatalog, ExtraLoadedTools: loaded,
		Conversational: req.Conversational,
	})
	if err != nil {
		return Launched{}, fmt.Errorf("start run: %w", err)
	}
	return Launched{SessionID: sessionID, Events: events}, nil
}
