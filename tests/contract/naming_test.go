package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// buildStage is the word the repository no longer lets name anything. The build
// ledger that numbered the work in phases is gone (specs/README.md says where each
// "Phase N" citation resolves), and a name that carries a phase number says when
// something was written instead of what it is: phase6Rig, "phase2-v1" and a file
// called phase5_oversight_test.go each had to be renamed by hand to say what they
// do. This rule keeps the next one out.
const buildStage = "phase"

// buildStageExempt names the files whose subject is the citation convention
// itself, keyed by slash-separated repo-relative path, valued by WHY. Prose
// comments are not checked at all: "Phase N" in a comment is the traceability
// convention TestSpecs_PhaseCitationsResolve enforces, and it is how a reader
// finds the spec a piece of code came from.
var buildStageExempt = map[string]string{
	"tests/contract/specs_test.go":  "parses the \"Phase N\" citations and the phase-to-spec table, so its names are its subject",
	"tests/contract/naming_test.go": "this rule",
}

// TestNoBuildStageNames fails when a file name, a Go identifier or a Go string
// literal contains the word for a build stage. Names and literals are what code
// is made of; comments are left to the citation convention above.
func TestNoBuildStageNames(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	files, goFiles := 0, 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" || name == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		files++
		if _, ok := buildStageExempt[rel]; ok {
			return nil
		}
		if strings.Contains(strings.ToLower(name), buildStage) {
			offenders = append(offenders, rel+": the file name names a build stage; name it for what it contains")
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		goFiles++
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if strings.Contains(strings.ToLower(v.Name), buildStage) {
					offenders = append(offenders, rel+": identifier "+v.Name+" names a build stage")
				}
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(v.Value)
				if uerr != nil {
					s = v.Value
				}
				if strings.Contains(strings.ToLower(s), buildStage) {
					offenders = append(offenders, rel+": string literal "+strconv.Quote(shorten(s))+" names a build stage")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files < 500 || goFiles < 300 {
		t.Fatalf("scanned %d files (%d Go); the rule would pass vacuously", files, goFiles)
	}
	slices.Sort(offenders)
	for _, o := range dedupe(offenders) {
		t.Error(o)
	}
}

func shorten(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
