package cost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoFloatInPackage is the package-local guard README task 4.1 and
// .golangci.yml's forbidigo comment both name explicitly: forbidigo matches
// call/selector expressions, so it cannot ban a TYPE (float32/float64) —
// this walks the AST of every non-test .go file in this package instead and
// fails the build if either identifier appears anywhere (a field type, a
// local var, a literal conversion, a parameter — any use at all). Every
// amount in this package is an exact integer Money (money.go); a float here
// would silently reintroduce the rounding drift Money.PriceQuantity exists
// to round out exactly once.
//
// _test.go files are excluded on purpose: a test may legitimately want a
// float to compute an expected value a different way than the code under
// test, as a cross-check — the ban is on the shipped package, not on how a
// test independently derives its assertions.
func TestNoFloatInPackage(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files matched — the guard would vacuously pass forever")
	}

	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "float32" || id.Name == "float64" {
				t.Errorf("%s: forbidden %s use at %s — internal/cost is exact-integer Money only (README task 4.1)",
					name, id.Name, fset.Position(id.Pos()))
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("every .go file in this package was a _test.go file — nothing was actually checked")
	}
}
