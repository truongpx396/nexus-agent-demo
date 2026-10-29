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

var rules = []boundaryRule{
	{
		name: "kernel must not import surfaces, controlplane, or an adapter of its own ports",
		pkg:  modulePrefix + "kernel",
		forbidden: []string{
			modulePrefix + "internal/surfaces",
			modulePrefix + "internal/controlplane",
			modulePrefix + "internal/eventlog/postgres",
			modulePrefix + "internal/eventlog/inmem",
		},
	},
	{
		// The kernel is the core of the hexagon: it reaches durable state only
		// through kernel.EventLog, never through the Postgres adapter's
		// plumbing. Checked on the kernel's OWN imports because it still
		// reaches internal/store and pgx transitively — internal/cost and
		// internal/obs each hold a Postgres-backed type next to the plain
		// types the kernel actually uses (cost.Money/Reservation, obs.Tracer).
		// Splitting those two packages is what would let this rule go
		// transitive; until then this is the strongest statement that holds.
		name: "kernel must not itself import internal/store or a database/cache driver",
		pkg:  modulePrefix + "kernel",
		forbidden: []string{
			modulePrefix + "internal/store",
			"github.com/jackc/pgx",
			"github.com/redis",
		},
		direct: true,
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
