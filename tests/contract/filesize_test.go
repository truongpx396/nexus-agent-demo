package contract

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// maxGoFileLines is the hard cap on a non-test Go source file (CLAUDE.md
// Conventions: "aim for ~150–400 lines per file"; this is the line a file
// must never cross). The check is mechanical for the same reason the
// import-boundary rules are: a convention that only lives in prose drifts.
// Test files are exempt — a table-driven or phase-scoped integration test
// legitimately reads top to bottom as one narrative.
const maxGoFileLines = 500

// lineCapExempt names files allowed past maxGoFileLines, keyed by
// slash-separated repo-relative path, valued by WHY. It is empty on purpose:
// the fix for an oversized file is to split it by responsibility within the
// same package (kernel/turns.go -> kernel/metering.go is the worked
// example), not to list it here. An entry needs a justification a reviewer
// would accept.
var lineCapExempt = map[string]string{}

var generatedHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

type oversizedFile struct {
	path  string
	lines int
}

// countLines counts lines the way `wc -l` does for newline-terminated files,
// and counts a final unterminated line too — so the .claude/hooks/ size
// nudge (which shells out to wc -l) and this test agree.
func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// isGenerated reports whether data carries the standard Go generated-file
// marker in its leading comment block (https://go.dev/s/generatedcode).
func isGenerated(data []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for i := 0; i < 20 && sc.Scan(); i++ {
		if generatedHeader.MatchString(sc.Text()) {
			return true
		}
	}
	return false
}

// scanGoFileSizes walks root and returns how many non-test, non-generated
// .go files it examined plus every one over limit (and not exempt). It skips
// dot-directories, node_modules, vendor and testdata.
func scanGoFileSizes(root string, limit int, exempt map[string]string) (checked int, over []oversizedFile, err error) {
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
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := exempt[rel]; ok {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over this repo's own checkout, never external input
		if err != nil {
			return err
		}
		if isGenerated(data) {
			return nil
		}
		checked++
		if n := countLines(data); n > limit {
			over = append(over, oversizedFile{path: rel, lines: n})
		}
		return nil
	})
	sort.Slice(over, func(i, j int) bool { return over[i].lines > over[j].lines })
	return checked, over, err
}

// moduleRoot finds the directory holding go.mod, walking up from the test's
// working directory (go test runs in the package directory).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the contract test directory")
		}
		dir = parent
	}
}

func TestGoFilesStayUnderLineCap(t *testing.T) {
	checked, over, err := scanGoFileSizes(moduleRoot(t), maxGoFileLines, lineCapExempt)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Fail loudly rather than pass vacuously, like the boundary and metering
	// checks: zero files examined means the walk is broken, not that the
	// repo is clean.
	if checked == 0 {
		t.Fatal("examined zero Go files; the walk is not finding the source tree")
	}
	for _, f := range over {
		t.Errorf("%s is %d lines (cap %d): split it by responsibility within the same package (CLAUDE.md Conventions), or add a justified entry to lineCapExempt", f.path, f.lines, maxGoFileLines)
	}
}

// TestScanGoFileSizes proves the scanner can actually fail: a cap check that
// has never been seen to flag anything is indistinguishable from one that is
// broken.
func TestScanGoFileSizes(t *testing.T) {
	lines := func(n int) string { return strings.Repeat("// x\n", n) }
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/at_cap.go", lines(10))
	write("pkg/over_cap.go", lines(11))
	write("pkg/big_test.go", lines(50))                                        // test file: exempt
	write("pkg/gen.go", "// Code generated by tool. DO NOT EDIT.\n"+lines(50)) // generated: skipped
	write("pkg/testdata/fixture.go", lines(50))                                // testdata: skipped
	write(".hidden/skip.go", lines(50))                                        // dot-dir: skipped
	write("pkg/allowed.go", lines(50))                                         // exempt by entry
	checked, over, err := scanGoFileSizes(root, 10, map[string]string{"pkg/allowed.go": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Errorf("checked = %d, want 2 (at_cap.go and over_cap.go)", checked)
	}
	if len(over) != 1 || over[0].path != "pkg/over_cap.go" || over[0].lines != 11 {
		t.Errorf("over = %+v, want exactly pkg/over_cap.go at 11 lines", over)
	}
}

func TestCountLines(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0}, {"a", 1}, {"a\n", 1}, {"a\nb", 2}, {"a\n\n", 2},
	} {
		if got := countLines([]byte(tc.in)); got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
