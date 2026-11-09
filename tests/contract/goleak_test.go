package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// goleakImportPath is the leak detector every goroutine-starting package's
// tests must run (go.uber.org/goleak).
const goleakImportPath = "go.uber.org/goleak"

// leakCheckExempt names packages that start a goroutine, have tests, and are
// allowed to skip the leak check, keyed by slash-separated repo-relative
// directory, valued by WHY. It is empty on purpose, like lineCapExempt: the fix
// for a package that fails the check is a one-line TestMain (see the failure
// message), and for a test that leaks, a t.Cleanup that cancels the context or
// closes the client, not an entry here.
var leakCheckExempt = map[string]string{}

type leakScan struct {
	startsGoroutine bool // a non-test file contains a go statement
	hasTests        bool // the directory has at least one _test.go file
	checksLeaks     bool // a _test.go file declares a TestMain that calls into goleak
}

// scanLeakChecks walks root and returns how many packages start a goroutine in
// non-test code and have tests (and are not exempt), plus those of them whose
// tests never call goleak from a TestMain. A package with no tests has nothing
// to attach a TestMain to, so it is not counted. It skips dot-directories,
// node_modules, vendor and testdata.
func scanLeakChecks(root string, exempt map[string]string) (checked int, missing []string, err error) {
	byDir := map[string]*leakScan{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		dir := filepath.ToSlash(rel)
		scan := byDir[dir]
		if scan == nil {
			scan = &leakScan{}
			byDir[dir] = scan
		}
		if strings.HasSuffix(name, "_test.go") {
			scan.hasTests = true
			scan.checksLeaks = scan.checksLeaks || declaresLeakCheckingTestMain(file)
			return nil
		}
		scan.startsGoroutine = scan.startsGoroutine || containsGoStmt(file)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	for dir, scan := range byDir {
		if !scan.startsGoroutine || !scan.hasTests {
			continue
		}
		if _, ok := exempt[dir]; ok {
			continue
		}
		checked++
		if !scan.checksLeaks {
			missing = append(missing, dir)
		}
	}
	sort.Strings(missing)
	return checked, missing, nil
}

func containsGoStmt(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			found = true
		}
		return !found
	})
	return found
}

// declaresLeakCheckingTestMain reports whether file imports goleak and has a
// TestMain whose body calls something from it (VerifyTestMain, Find, ...). A
// TestMain that never touches goleak does not count.
func declaresLeakCheckingTestMain(file *ast.File) bool {
	local := ""
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != goleakImportPath {
			continue
		}
		local = "goleak"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" || local == "." {
		return false
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// TestGoroutineStartingPackagesCheckForLeaks is the mechanical form of "a
// goroutine that outlives its test is a bug the test should have caught": every
// package that starts a goroutine in non-test code runs goleak after its tests.
func TestGoroutineStartingPackagesCheckForLeaks(t *testing.T) {
	checked, missing, err := scanLeakChecks(moduleRoot(t), leakCheckExempt)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Fail loudly rather than pass vacuously, like the other contract checks:
	// zero packages examined means the walk is broken, not that the repo is clean.
	if checked == 0 {
		t.Fatal("examined zero goroutine-starting packages with tests; the walk is not finding the source tree")
	}
	for _, dir := range missing {
		t.Errorf("%s starts a goroutine but its tests do not run goleak: add %s/leak_test.go containing\n\tfunc TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n(import %q), or add a justified entry to leakCheckExempt", dir, dir, goleakImportPath)
	}
}

func writeFixture(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestScanLeakChecks proves the scanner can actually fail, and that each way of
// not having a real check (no TestMain, a TestMain that ignores goleak) is seen.
func TestScanLeakChecks(t *testing.T) {
	spawn := "package p\n\nfunc F() { go func() {}() }\n"
	quiet := "package p\n\nfunc F() {}\n"
	files := map[string]string{
		// starts a goroutine, no TestMain at all: flagged.
		"spawns/a.go":      spawn,
		"spawns/a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		// starts a goroutine, TestMain calls goleak: fine.
		"checked/b.go":      spawn,
		"checked/b_test.go": "package p\n\nimport (\n\t\"testing\"\n\n\t\"go.uber.org/goleak\"\n)\n\nfunc TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n",
		// starts a goroutine, TestMain never calls goleak: flagged.
		"fake/e.go":      spawn,
		"fake/e_test.go": "package p\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
		// an aliased import still counts.
		"aliased/f.go":      spawn,
		"aliased/f_test.go": "package p\n\nimport (\n\t\"testing\"\n\n\tleak \"go.uber.org/goleak\"\n)\n\nfunc TestMain(m *testing.M) { leak.VerifyTestMain(m) }\n",
		// no goroutine started: not this rule's business.
		"quiet/c.go":      quiet,
		"quiet/c_test.go": "package p\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n",
		// starts a goroutine but has no tests: nothing to attach a TestMain to.
		"notests/d.go": spawn,
		// an exempted package is not checked.
		"exempt/g.go":      spawn,
		"exempt/g_test.go": "package p\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n",
	}
	root := t.TempDir()
	for rel, body := range files {
		writeFixture(t, root, rel, body)
	}

	checked, missing, err := scanLeakChecks(root, map[string]string{"exempt": "fixture"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if checked != 4 {
		t.Errorf("checked = %d, want 4 (spawns, checked, fake, aliased)", checked)
	}
	if want := []string{"fake", "spawns"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
}
