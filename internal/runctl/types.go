// Package runctl implements README task 6.9's four named operations —
// steer, cancel, resume, tightenAutonomy — plus replay (task 6.10) and fork
// (task 6.11), and the durable Claims tracker (task 6.6) that bridges
// internal/store's claims-table CRUD to internal/tools.Claims (sealing and
// appending EventEffectClaimed/EventEffectClaimResolved needs
// internal/crypto, which internal/store cannot import without a cycle — see
// internal/store/claims.go's own doc comment).
//
// runctl is free to import kernel and internal/oversight directly — only
// kernel's own import allowlist is restricted (kernel/types.go); nothing
// stops a package the kernel doesn't import from importing the kernel, the
// same rule internal/oversight's own package doc comment already states.
package runctl

import (
	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Control bundles every collaborator this package's operations need.
// System/Catalog/MaxTurns mirror internal/oversight.Resumer's own fields
// exactly, for the same reason that file's doc comment gives: no
// per-tenant config store exists yet (Phase 7's internal/config), so one
// process-wide system prompt and resident catalog cover every session
// regardless of which one this operation targets.
type Control struct {
	Store     *store.Store
	Keys      *crypto.KeyStore
	Chain     *audit.Chain
	Approvals *oversight.Approvals
	Inputs    *oversight.Inputs
	Kernel    *kernel.Kernel
	System    string
	Catalog   []provider.ToolSchema
	MaxTurns  int

	// CatalogManifestDigest folds into a forked session's harness_digest
	// (Fork, README task 6.11) exactly the way
	// internal/surfaces/rest/server.go's own run-creation path already
	// folds it into a fresh session's — cmd/nexusd wires the SAME value
	// into both, so a fork taken with no overrides at all reproduces the
	// parent's digest exactly (never "diverged" merely because two
	// unrelated call sites computed it differently).
	CatalogManifestDigest []byte
}

// deps is the small (store, keys, chain) subset several of Control's own
// methods need to seal and append an out-of-band event — mirroring
// internal/oversight's own unexported `deps` type field-for-field, so
// claims.go's appendEvent can be a near-identical copy of oversight's own
// (duplicated rather than shared across packages for the same reason
// oversight/invalidate.go already duplicates kernel's toolResultPayload
// shape: two packages with no other reason to depend on each other).
type deps struct {
	Store *store.Store
	Keys  *crypto.KeyStore
	Chain *audit.Chain
}

func (c *Control) deps() deps { return deps{Store: c.Store, Keys: c.Keys, Chain: c.Chain} }
