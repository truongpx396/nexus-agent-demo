// Package contract holds tests that pin the seams README.md §4 declares —
// starting with the import-boundary rule that keeps the control-plane /
// data-plane split (and the surface/kernel separation Principle I demands)
// real rather than aspirational: "splitting into processes later is a
// main.go change, not a rewrite" is only true if nothing already reaches
// across the boundary.
package contract

import (
	"testing"

	"golang.org/x/tools/go/packages"
)

const modulePrefix = "github.com/truongpx396/nexus-agent-demo/"

// boundaryRule declares that pkg must never import any path under
// forbidden. Rules name packages that do not exist yet (kernel/,
// internal/surfaces, internal/controlplane land in Phase 2/7) on purpose:
// packages.Load reports zero matches for a path that doesn't exist yet, so
// the rule is a structural no-op until that phase's package lands — nobody
// has to remember to add the check later.
type boundaryRule struct {
	name      string
	pkg       string
	forbidden []string
}

var rules = []boundaryRule{
	{
		name: "kernel must not import surfaces or controlplane",
		pkg:  modulePrefix + "kernel",
		forbidden: []string{
			modulePrefix + "internal/surfaces",
			modulePrefix + "internal/controlplane",
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
		name: "internal/version is a leaf: it must not import anything else in this module",
		pkg:  modulePrefix + "internal/version",
		forbidden: []string{
			modulePrefix + "internal",
			modulePrefix + "cmd",
			modulePrefix + "kernel",
		},
	},
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

			imported := map[string]bool{}
			packages.Visit(pkgs, func(p *packages.Package) bool {
				imported[p.PkgPath] = true
				return true
			}, nil)

			for _, forbidden := range r.forbidden {
				if imported[forbidden] {
					t.Errorf("%s imports %s, which violates a boundary rule in README.md §4", r.pkg, forbidden)
				}
			}
		})
	}

	if checked == 0 {
		t.Fatal("no boundary rule matched an existing package — this test would silently pass forever; check the package paths above")
	}
}
