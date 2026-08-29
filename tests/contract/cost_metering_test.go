package contract

import (
	"go/ast"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestEveryProviderStreamCallIsMetered is the AST check README.md §5's
// Risks table names for task 4.8: "Cost metering gets bolted onto
// foreground turns only ... a test asserts every Provider.Stream caller in
// the codebase passes through BudgetGate.Reserve — enforced by an AST
// check, not by review." It loads the whole module with real type
// information, finds every call that resolves to
// internal/provider.Provider's Stream method, and requires the innermost
// enclosing function to also contain a call named "Reserve" at an earlier
// source position — internal/cost.Gate.Reserve today (kernel/loop.go's
// turn loop is the one real call site this phase), whatever else
// implements kernel.BudgetGate later (a future compaction/safety-model/
// hook-prompt/judge/title call site — see internal/cost.Purpose's own doc
// comment).
//
// The "named Reserve" half of this check is a plain identifier match, not
// itself type-checked against cost.Gate: precise enough for this
// codebase's actual shape, since nothing else exports a method called
// Reserve. A name collision would show up as a false negative (a real
// unmetered Stream call passing this test); it would not show up as a
// false positive, so this test failing is always worth investigating, and
// it passing is only as strong as that one assumption — documented here
// rather than solved with full call-graph analysis, which is more
// machinery than a single-call-site codebase needs yet.
func TestEveryProviderStreamCallIsMetered(t *testing.T) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Fset: fset,
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}
	pkgs, err := packages.Load(cfg, modulePrefix+"...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}

	const providerPkgPath = modulePrefix + "internal/provider"

	checked := 0
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue // a test-only or otherwise synthetic package variant; nothing to check
		}
		for _, file := range pkg.Syntax {
			reserveCalls := collectNamedCallPositions(file, "Reserve")

			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Stream" {
					return true
				}
				fn, ok := pkg.TypesInfo.ObjectOf(sel.Sel).(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != providerPkgPath {
					return true // some other selector named "Stream" (e.g. an io.Reader) — not our concern
				}
				if pkg.PkgPath == providerPkgPath {
					// internal/provider/failover.go's own retry loop calls
					// prov.Stream on each underlying candidate — that is
					// provider.Wrap FULFILLING one already-reserved logical
					// call (kernel/loop.go's single Reserve covers the
					// whole k.Provider.Stream call, retries included), not
					// a second, independently unmetered one. The check
					// below is about consumers of the Provider
					// abstraction reaching in around Reserve, not the
					// abstraction's own internal plumbing.
					return true
				}
				checked++

				fnStart, found := enclosingFuncStart(file, call.Pos())
				if !found {
					t.Errorf("%s: Provider.Stream call at %s is not inside any function body (unexpected)",
						pkg.PkgPath, fset.Position(call.Pos()))
					return true
				}

				metered := false
				for _, rp := range reserveCalls {
					if rp >= fnStart && rp < call.Pos() {
						metered = true
						break
					}
				}
				if !metered {
					t.Errorf("%s: Provider.Stream call at %s has no preceding Reserve call in its enclosing function — every model call must be metered (README.md §5, task 4.8)",
						pkg.PkgPath, fset.Position(call.Pos()))
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("no Provider.Stream call site was found anywhere in the module — this test would silently pass forever; check the type-matching logic above")
	}
}

// collectNamedCallPositions returns the source position of every call
// expression in file whose selector is named name (e.g. "Reserve"),
// regardless of receiver type — see this test's own doc comment on why a
// name-based match is precise enough here.
func collectNamedCallPositions(file *ast.File, name string) []token.Pos {
	var positions []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		positions = append(positions, call.Pos())
		return true
	})
	return positions
}

// enclosingFuncStart returns the start position of the innermost
// FuncDecl/FuncLit whose range contains pos. ast.Inspect visits a node
// before its children, so a nested FuncLit inside an outer FuncDecl is
// necessarily visited AFTER that outer one — when both ranges contain pos,
// the nested (tighter) one is visited later and correctly overwrites the
// result, which is what makes the last write here always the innermost
// match rather than needing an explicit smallest-range comparison.
func enclosingFuncStart(file *ast.File, pos token.Pos) (start token.Pos, found bool) {
	ast.Inspect(file, func(n ast.Node) bool {
		var s, e token.Pos
		switch fn := n.(type) {
		case *ast.FuncDecl:
			s, e = fn.Pos(), fn.End()
		case *ast.FuncLit:
			s, e = fn.Pos(), fn.End()
		default:
			return true
		}
		if s <= pos && pos <= e {
			start, found = s, true
		}
		return true
	})
	return
}
