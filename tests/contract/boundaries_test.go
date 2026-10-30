// Package contract holds tests that pin the seams README.md §4 declares —
// starting with the import-boundary rule that keeps the control-plane /
// data-plane split (and the surface/kernel separation Principle I demands)
// real rather than aspirational: "splitting into processes later is a
// main.go change, not a rewrite" is only true if nothing already reaches
// across the boundary.
package contract

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

const modulePrefix = "github.com/truongpx396/nexus-agent-demo/"

// boundaryRule declares that pkg must never import — directly or
// transitively — any path under forbidden. A forbidden entry P forbids P
// itself and every package below P/, so a rule can name a whole tree
// (".../internal/surfaces") without listing each adapter, and a package added
// to that tree later is covered the day it lands. Rules name packages that do not exist yet (kernel/,
// internal/surfaces, internal/controlplane land in Phase 2/7) on purpose:
// packages.Load reports zero matches for a path that doesn't exist yet, so
// the rule is a structural no-op until that phase's package lands — nobody
// has to remember to add the check later.
type boundaryRule struct {
	name      string
	pkg       string
	forbidden []string
	// direct restricts the check to pkg's own import declarations instead of
	// its whole transitive closure. Use it for a rule that is true of the
	// package itself today but cannot yet hold transitively because a
	// dependency it legitimately imports still reaches the forbidden path.
	direct bool
}

// noStorage is the rule for a package that must reach neither the store nor a
// database/cache driver, even transitively. The surfaces also have DIRECT rules
// below that keep them out of internal/crypto altogether (they never seal or
// open anything themselves); this is the stronger statement that nothing they
// import brings a driver along.
func noStorage(subject, pkg string) boundaryRule {
	return boundaryRule{
		name: subject + " must not reach the database or cache",
		pkg:  modulePrefix + pkg,
		forbidden: []string{
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	}
}

func init() {
	for _, s := range []struct{ subject, pkg string }{
		{"the REST surface", "internal/surfaces/rest"},
		{"the telegram surface", "internal/surfaces/telegram"},
		{"the zalo surface", "internal/surfaces/zalo"},
		{"the email surface", "internal/surfaces/email"},
		{"the cron surface", "internal/surfaces/cron"},
		{"the mcp surface", "internal/surfaces/mcp"},
		{"the control plane port", "internal/controlplane"},
		{"the tool pipeline", "internal/tools"},
		{"run control", "internal/runctl"},
		{"the job queue port", "internal/queue"},
		{"the oversight service", "internal/oversight"},
	} {
		rules = append(rules, noStorage(s.subject, s.pkg))
	}
}

var rules = []boundaryRule{
	{
		name: "kernel must not import surfaces, controlplane, an adapter of its own ports, or reach the database or cache",
		pkg:  modulePrefix + "kernel",
		forbidden: []string{
			modulePrefix + "internal/surfaces",
			modulePrefix + "internal/controlplane",
			modulePrefix + "internal/eventlog/postgres",
			modulePrefix + "internal/eventlog/inmem",
			// The core of the hexagon reaches durable state only through its
			// ports (kernel.EventLog, kernel.BudgetGate). Transitive: every
			// package it imports for vocabulary (cost, obs, ...) is likewise
			// free of storage, with its adapters in a subpackage.
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// Telemetry and the content-access service (Tracer, Attrs, Grants,
		// Reports) are reached by the kernel and by REST. Their Postgres
		// adapters are internal/obs/postgres.
		name: "obs must not reach the database or cache",
		pkg:  modulePrefix + "internal/obs",
		forbidden: []string{
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// The envelope (DEK, KEK, Seal, Open, the erasure vocabulary) is used
		// everywhere events are sealed, so it must not drag a driver along.
		// Its Postgres KeyStore and crypto-shredding are internal/crypto/postgres.
		name: "crypto must not reach the database or cache",
		pkg:  modulePrefix + "internal/crypto",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/audit/postgres",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// The receipt vocabulary, the hash and the signer protocol. The chain
		// that appends, anchors and verifies receipts in a transaction is
		// internal/audit/postgres.
		name: "audit must not reach the database or cache",
		pkg:  modulePrefix + "internal/audit",
		forbidden: []string{
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// The budget domain (Money, Gate, the Ledger and Counters ports) is
		// reached by the kernel, so it must stay free of its own storage.
		// Its Postgres and Redis adapters are internal/cost/postgres and
		// internal/cost/redis; the in-memory ones are internal/cost/inmem.
		name: "cost must not reach the database or cache",
		pkg:  modulePrefix + "internal/cost",
		forbidden: []string{
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// A "/..." pattern, not the bare package path: internal/surfaces
		// itself has no .go files of its own — every surface lives one
		// level down (internal/surfaces/rest landed in Phase 2;
		// internal/surfaces/cli is Phase 7), and go/packages only loads a
		// directory that actually contains a package. The bare path would
		// silently report "doesn't exist yet" forever and never check
		// anything; the wildcard follows the tree as surfaces are added.
		name: "surfaces must not import the kernel directly",
		pkg:  modulePrefix + "internal/surfaces/...",
		forbidden: []string{
			modulePrefix + "kernel",
		},
	},
	{
		// internal/runs is the one package every surface shares. If it could
		// reach the kernel, so could every surface — and the failure would
		// surface as a confusing violation of the surfaces rule instead.
		name: "runs must not import the kernel or any surface",
		pkg:  modulePrefix + "internal/runs",
		forbidden: []string{
			modulePrefix + "kernel",
			modulePrefix + "internal/surfaces",
		},
	},
	{
		// runs is the application layer: it states the run-admission use case
		// over ports (Admitter, KeyProvider, Starter) and leaves Postgres to
		// internal/runs/postgres. Transitive: the envelope it imports for
		// SealFor (internal/crypto) has no storage of its own — the KeyStore
		// is internal/crypto/postgres.
		name: "runs must not reach the database or cache",
		pkg:  modulePrefix + "internal/runs",
		forbidden: []string{
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// The REST surface starts runs through runs.Launcher and reads them
		// back through runs.Reader, so it has no reason to touch the database
		// or the key store itself. The Telegram, Zalo and email surfaces
		// below follow the same rule: they find a conversation with
		// Reader.FindByKey, open the events a notification quotes with
		// Reader.EventOpener, and dedupe redeliveries through
		// surfaces.InboundDedup. Cron and MCP follow below, so no surface
		// touches the database directly any more.
		name: "the REST surface must not itself import internal/store, internal/crypto or a database/cache driver",
		pkg:  modulePrefix + "internal/surfaces/rest",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
	},
	{
		// The primitives every surface shares (the delivery outbox, inbound
		// dedup) state their policy over ports; their Postgres adapters are
		// internal/surfaces/postgres. Transitive, unlike the per-surface rules
		// below, because nothing it imports reaches a driver.
		name: "the shared surfaces package must not reach the database, the key store or the cache",
		pkg:  modulePrefix + "internal/surfaces",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			modulePrefix + "internal/audit",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
	{
		// The scheduler decides which schedules are valid, the route, the
		// autonomy default and when the next run is; ScheduleStore does the
		// rest, with the session-plus-schedule step as ONE method so the tick
		// stays exactly-once. Its adapter is internal/surfaces/cron/postgres.
		name: "the cron surface must not itself import internal/store, internal/crypto or a database/cache driver",
		pkg:  modulePrefix + "internal/surfaces/cron",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
	},
	{
		// The MCP resolver states what resolves (an admitted server whose
		// live schema still matches the ref), as whom a server is called, and
		// how long a tool list is cached; ServerDirectory holds the rows and
		// opens a sealed credential where its key lives. Its adapter is
		// internal/surfaces/mcp/postgres.
		name: "the mcp surface must not itself import internal/store, internal/crypto or a database/cache driver",
		pkg:  modulePrefix + "internal/surfaces/mcp",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
	},
	{
		name: "the telegram surface must not itself import internal/store, internal/crypto or a database/cache driver",
		pkg:  modulePrefix + "internal/surfaces/telegram",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
	},
	{
		name: "the zalo surface must not itself import internal/store, internal/crypto or a database/cache driver",
		pkg:  modulePrefix + "internal/surfaces/zalo",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
	},
	{
		name: "the email surface must not itself import internal/store, internal/crypto or a database/cache driver",
		pkg:  modulePrefix + "internal/surfaces/email",
		forbidden: []string{
			modulePrefix + "internal/store",
			modulePrefix + "internal/crypto",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
	},
	{
		name: "controlplane must not import data-plane-only packages",
		pkg:  modulePrefix + "internal/controlplane",
		forbidden: []string{
			modulePrefix + "internal/sandbox",
			modulePrefix + "internal/memory",
			modulePrefix + "internal/provider",
		},
	},
	{
		name: "nexusctl must not import the other binaries",
		pkg:  modulePrefix + "cmd/nexusctl",
		forbidden: []string{
			modulePrefix + "cmd/nexusd",
			modulePrefix + "cmd/signerd",
		},
	},
	{
		name: "nexusd must not import the other binaries",
		pkg:  modulePrefix + "cmd/nexusd",
		forbidden: []string{
			modulePrefix + "cmd/nexusctl",
			modulePrefix + "cmd/signerd",
		},
	},
	{
		// README.md §5, task 5.1: "nexusd can sign but cannot read the
		// key." internal/audit/signerkey holds the Ed25519 private key and
		// the only code that signs with it; cmd/signerd is the sole
		// importer. nexusd only ever reaches signing through
		// internal/audit.SignerClient's unix-socket RPC (internal/audit/
		// signer.go) — this rule is what makes "cannot read the key" a
		// property of the build graph, not a convention nexusd could
		// accidentally violate by adding one import.
		name: "nexusd must not import the signer's private key package",
		pkg:  modulePrefix + "cmd/nexusd",
		forbidden: []string{
			modulePrefix + "internal/audit/signerkey",
		},
	},
	{
		name: "internal/version is a leaf: it must not import anything else in this module",
		pkg:  modulePrefix + "internal/version",
		forbidden: []string{
			modulePrefix + "internal",
			modulePrefix + "cmd",
			modulePrefix + "kernel",
		},
	},
	{
		// The event-log domain model is the innermost layer of the hexagon:
		// kernel/ names its types without depending on how events are stored.
		// Keeping it free of this module and of every database/cache driver is
		// what lets internal/store stay one adapter among several.
		name: "internal/eventlog is a persistence-free leaf: no module packages, no database or cache drivers",
		pkg:  modulePrefix + "internal/eventlog",
		forbidden: []string{
			modulePrefix + "internal",
			modulePrefix + "cmd",
			modulePrefix + "kernel",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
	},
}

// violates reports whether imported falls under forbidden: that package
// itself or anything below it.
func violates(imported, forbidden string) bool {
	return imported == forbidden || strings.HasPrefix(imported, forbidden+"/")
}

func TestImportBoundaries(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps}

	checked := 0
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			pkgs, err := packages.Load(cfg, r.pkg)
			if err != nil {
				t.Fatalf("packages.Load(%s): %v", r.pkg, err)
			}
			if len(pkgs) == 0 || pkgs[0].Name == "" {
				t.Skipf("%s does not exist yet — rule activates once it lands", r.pkg)
				return
			}
			checked++

			// The rule's own packages are roots, not imports: a "leaf" rule
			// that forbids the whole module must not trip over the package
			// it is guarding.
			roots := map[string]bool{}
			for _, p := range pkgs {
				roots[p.PkgPath] = true
			}
			var imported []string
			if r.direct {
				for _, p := range pkgs {
					for path := range p.Imports {
						imported = append(imported, path)
					}
				}
			} else {
				packages.Visit(pkgs, func(p *packages.Package) bool {
					if !roots[p.PkgPath] {
						imported = append(imported, p.PkgPath)
					}
					return true
				}, nil)
			}
			sort.Strings(imported)

			for _, forbidden := range r.forbidden {
				var hits []string
				for _, path := range imported {
					if violates(path, forbidden) {
						hits = append(hits, path)
					}
				}
				if len(hits) > 0 {
					t.Errorf("%s imports %s (%d package(s) under %s in all), which violates a boundary rule in README.md §4",
						r.pkg, hits[0], len(hits), forbidden)
				}
			}
		})
	}

	if checked == 0 {
		t.Fatal("no boundary rule matched an existing package — this test would silently pass forever; check the package paths above")
	}
}
