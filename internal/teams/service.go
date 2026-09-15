package teams

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
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
