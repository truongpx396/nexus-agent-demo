// Package runctl implements README task 6.9's four named operations —
// steer, cancel, resume, tightenAutonomy — plus replay (task 6.10) and fork
// (task 6.11), and the durable Claims tracker (task 6.6) that bridges
// claims-table ledger to internal/tools.Claims, appending EventEffectClaimed/
// EventEffectClaimResolved in the same unit of work as the claim it records.
//
// runctl is free to import kernel and internal/oversight directly — only
// kernel's own import allowlist is restricted (kernel/types.go); nothing
// stops a package the kernel doesn't import from importing the kernel, the
// same rule internal/oversight's own package doc comment already states.
package runctl

import (
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Control bundles every collaborator this package's operations need.
// System/Catalog/MaxTurns mirror internal/oversight.Resumer's own fields
// exactly, for the same reason that file's doc comment gives: no
// per-tenant config store exists yet (Phase 7's internal/config), so one
// process-wide system prompt and resident catalog cover every session
// regardless of which one this operation targets.
type Control struct {
	Store     Store
	Approvals *oversight.Approvals
	Inputs    *oversight.Inputs
	Kernel    *kernel.Kernel
	System    string
	Catalog   []provider.ToolSchema
	MaxTurns  int

	// Queue is where ResumeConversation enqueues the queue.KindConverse job
	// that actually drives the turn loop, once it has durably appended the
	// human's message itself (the same "append now, run the loop later, in
	// a worker" split cmd/nexusd's kernelRunStarter uses for a fresh run's
	// own Kernel.Seed). Nil is valid only for callers that never invoke
	// ResumeConversation (every pre-this-change caller/test).
	Queue queue.Port

	// CatalogManifestDigest folds into a forked session's harness_digest
	// (Fork, README task 6.11) exactly the way
	// internal/surfaces/rest/server.go's own run-creation path already
	// folds it into a fresh session's — cmd/nexusd wires the SAME value
	// into both, so a fork taken with no overrides at all reproduces the
	// parent's digest exactly (never "diverged" merely because two
	// unrelated call sites computed it differently).
	CatalogManifestDigest []byte
}
