package contract

import (
	"go/ast"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestEveryEmbedderEmbedCallIsMetered is cost_metering_test.go's own AST
// check, extended to the embedding call site (README.md §5's Phase 12
// acceptance criterion, task 12.4: "a test that no embedding call bypasses
// BudgetGate.Reserve — the same AST-level check #4.8 already runs, extended
// to the embedding call site"). It is structurally the SAME check as
// TestEveryProviderStreamCallIsMetered, walking every call that resolves to
// internal/provider.Embedder's Embed method and requiring a preceding call
// named "Reserve" in the same enclosing function — internal/retrieval.
// Retriever.embed (internal/retrieval/retriever.go) is the one real call
// site this phase adds; see that test's own doc comment for the precision
// caveat a name-based "Reserve" match carries, which applies identically
// here.
func TestEveryEmbedderEmbedCallIsMetered(t *testing.T) {
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
			continue
		}
		for _, file := range pkg.Syntax {
			reserveCalls := collectNamedCallPositions(file, "Reserve")

			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Embed" {
					return true
				}
				fn, ok := pkg.TypesInfo.ObjectOf(sel.Sel).(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != providerPkgPath {
					return true // some other selector named "Embed" — not our concern
				}
				checked++

				fnStart, found := enclosingFuncStart(file, call.Pos())
				if !found {
					t.Errorf("%s: Embedder.Embed call at %s is not inside any function body (unexpected)",
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
					t.Errorf("%s: Embedder.Embed call at %s has no preceding Reserve call in its enclosing function — every embedding call must be metered (README.md §5, task 12.4)",
						pkg.PkgPath, fset.Position(call.Pos()))
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("no Embedder.Embed call site was found anywhere in the module — this test would silently pass forever; check the type-matching logic above")
	}
}
