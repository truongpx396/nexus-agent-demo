package contract

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The specs (specs/*/) are the single source of truth for what is built and what is open; the build ledger they replaced is deleted. What keeps them
// from drifting the way a hand-kept ledger did is this file: the structure is checked on every run of the unit tests. It cannot check that a sentence is
// true (that is /speckit-analyze and /speckit-converge), only that the ids, paths and files the sentences lean on still exist.
// specs/README.md says the same thing from the reader's side.

var (
	bpMarker     = regexp.MustCompile(`\(bp ([^)]*)\)`)
	dotID        = regexp.MustCompile(`\d+\.\d+`)
	taskCitation = regexp.MustCompile(`\b[Tt]asks? (\d+\.\d+)`)
	phaseCite    = regexp.MustCompile(`\bPhase (\d+)\b`)
	phaseRow     = regexp.MustCompile(`(?m)^\| (\d+) \| (\d{3}) \|`)
	aliasRow     = regexp.MustCompile(`(?m)^\| (\d+\.\d+) \| (\d+\.\d+) \|`)
	backticked   = regexp.MustCompile("`([^`\\s]+)`")
	specDirName  = regexp.MustCompile(`^\d{3}-`)
	mdLink       = regexp.MustCompile(`\]\(([^)#\s]+)(?:#[^)]*)?\)`)
)

// bpIDs returns every ledger id a tasks.md marks as `(bp N.M)`.
func bpIDs(tasks string) []string {
	var ids []string
	for _, m := range bpMarker.FindAllStringSubmatch(tasks, -1) {
		ids = append(ids, dotID.FindAllString(m[1], -1)...)
	}
	return ids
}

// resolves reports whether a `task id` citation lands on a task or a legacy alias.
func resolves(id string, known map[string]bool, aliases map[string]string) bool {
	if known[id] {
		return true
	}
	target, ok := aliases[id]
	return ok && known[target]
}

// pathTokens returns the backticked repository paths in a line. Route names (/healthz), tool ids (platform/delegate), URLs, globs, the untracked runtime
// directory .dev/ and anything with a placeholder are not paths and are skipped.
func pathTokens(line string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(line, -1) {
		p, _, _ := strings.Cut(strings.TrimRight(m[1], ",.;:"), "#")
		if !strings.Contains(p, "/") {
			continue
		}
		if strings.HasPrefix(p, "http") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "$") || strings.HasPrefix(p, "~") ||
			strings.HasPrefix(p, ".dev/") || strings.HasPrefix(p, "platform/") || strings.HasPrefix(p, "mcp/") || strings.HasPrefix(p, "nexus") ||
			strings.ContainsAny(p, "{<>*|@=(") {
			continue
		}
		if !strings.HasSuffix(p, "/") && !regexp.MustCompile(`\.\w{1,5}$`).MatchString(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func readSpecs(t *testing.T, root string) (dirs []string, tasks map[string]string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "specs"))
	if err != nil {
		t.Fatalf("read specs/: %v", err)
	}
	tasks = map[string]string{}
	for _, e := range entries {
		if !e.IsDir() || !specDirName.MatchString(e.Name()) {
			continue
		}
		dirs = append(dirs, e.Name())
		if b, err := os.ReadFile(filepath.Join(root, "specs", e.Name(), "tasks.md")); err == nil { //nolint:gosec // path is built from this repo's own specs/ directory listing, never external input
			tasks[e.Name()] = string(b)
		}
	}
	return dirs, tasks
}

func knownIDs(tasks map[string]string) map[string]bool {
	known := map[string]bool{}
	for _, body := range tasks {
		for _, id := range bpIDs(body) {
			known[id] = true
		}
	}
	return known
}

// specsIndex parses the two resolver tables in specs/README.md.
func specsIndex(t *testing.T, root string) (phases map[string]string, aliases map[string]string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "specs", "README.md")) //nolint:gosec // a fixed path under this repo's own checkout
	if err != nil {
		t.Fatalf("read specs/README.md: %v", err)
	}
	text := string(b)
	phases, aliases = map[string]string{}, map[string]string{}
	for _, m := range phaseRow.FindAllStringSubmatch(text, -1) {
		phases[m[1]] = m[2]
	}
	if i := strings.Index(text, "## Legacy task-number aliases"); i >= 0 {
		for _, m := range aliasRow.FindAllStringSubmatch(text[i:], -1) {
			aliases[m[1]] = m[2]
		}
	}
	return phases, aliases
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "dist": true, ".specify": true, "bin": true, ".dev": true}

// citingFiles are the files whose `task N.M` and `Phase N` citations must resolve: code, docs and config, not vendored or generated output, and not
// the Spec Kit scaffolding or skills (which are upstream text).
func citingFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if skipDirs[d.Name()] || rel == filepath.Join(".claude", "skills") || rel == "specs" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".yml", ".yaml", ".ts", ".tsx", ".sh":
			files = append(files, path)
		default:
			if b := d.Name(); b == "Makefile" || b == "Dockerfile" {
				files = append(files, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return files
}

func TestSpecs_EveryDirectoryHasTheThreeFiles(t *testing.T) {
	root := moduleRoot(t)
	dirs, _ := readSpecs(t, root)
	if len(dirs) == 0 {
		t.Fatal("no spec directories found under specs/")
	}
	for _, d := range dirs {
		for _, f := range []string{"spec.md", "plan.md", "tasks.md"} {
			if _, err := os.Stat(filepath.Join(root, "specs", d, f)); err != nil {
				t.Errorf("specs/%s is missing %s", d, f)
			}
		}
	}
}

// A ledger id belongs to exactly one spec, or "task N.M" would not say where to look.
func TestSpecs_EachLedgerIDBelongsToOneSpec(t *testing.T) {
	_, tasks := readSpecs(t, moduleRoot(t))
	owner := map[string]string{}
	for dir, body := range tasks {
		for _, id := range bpIDs(body) {
			if prev, ok := owner[id]; ok && prev != dir {
				t.Errorf("(bp %s) is marked in both %s and %s", id, prev, dir)
			}
			owner[id] = dir
		}
	}
}

func TestSpecs_TaskCitationsResolve(t *testing.T) {
	root := moduleRoot(t)
	_, tasks := readSpecs(t, root)
	_, aliases := specsIndex(t, root)
	known := knownIDs(tasks)
	var bad []string
	for _, f := range citingFiles(t, root) {
		b, err := os.ReadFile(f) //nolint:gosec // path comes from WalkDir over this repo's own checkout, never external input
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range taskCitation.FindAllStringSubmatch(string(b), -1) {
			if !resolves(m[1], known, aliases) {
				rel, _ := filepath.Rel(root, f)
				bad = append(bad, rel+": task "+m[1])
			}
		}
	}
	sort.Strings(bad)
	for _, b := range uniq(bad) {
		t.Errorf("%s resolves to no (bp) id in specs/*/tasks.md and is not a legacy alias in specs/README.md", b)
	}
}

func TestSpecs_PhaseCitationsResolve(t *testing.T) {
	root := moduleRoot(t)
	phases, _ := specsIndex(t, root)
	if len(phases) == 0 {
		t.Fatal("specs/README.md has no Phase-to-spec table")
	}
	var bad []string
	for _, f := range citingFiles(t, root) {
		if filepath.Ext(f) != ".go" {
			continue
		}
		b, _ := os.ReadFile(f) //nolint:gosec // path comes from WalkDir over this repo's own checkout, never external input
		for _, m := range phaseCite.FindAllStringSubmatch(string(b), -1) {
			if _, ok := phases[m[1]]; !ok {
				rel, _ := filepath.Rel(root, f)
				bad = append(bad, rel+": Phase "+m[1])
			}
		}
	}
	for _, b := range uniq(bad) {
		t.Errorf("%s is not in the Phase-to-spec table in specs/README.md", b)
	}
}

// A spec whose tasks.md declares "**Paths**: verified" makes a promise: every file a done task names exists. The retroactive specs declare their paths
// "as shipped (history)" instead, because a later refactor may have moved them.
func TestSpecs_DoneTasksNamePathsThatExist(t *testing.T) {
	root := moduleRoot(t)
	_, tasks := readSpecs(t, root)
	for dir, body := range tasks {
		if !strings.Contains(body, "**Paths**: verified") {
			continue
		}
		for line := range strings.SplitSeq(body, "\n") {
			if !strings.HasPrefix(line, "- [X]") {
				continue
			}
			for _, p := range pathTokens(line) {
				full := filepath.Join(root, p)
				if _, err := os.Stat(full); err == nil {
					continue
				}
				if m, _ := filepath.Glob(full); len(m) > 0 {
					continue
				}
				t.Errorf("specs/%s/tasks.md: %q names %s, which does not exist", dir, strings.Fields(line)[2], p)
			}
		}
	}
}

// A relative Markdown link in a spec that points nowhere is how a moved file goes unnoticed.
func TestSpecs_RelativeLinksResolve(t *testing.T) {
	root := moduleRoot(t)
	specsDir := filepath.Join(root, "specs")
	err := filepath.WalkDir(specsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over this repo's own specs/, never external input
		if err != nil {
			return err
		}
		for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
			target := m[1]
			if strings.HasPrefix(target, "http") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "/") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), target)); err != nil { //nolint:gosec // the target is a link written in this repo's own specs/, resolved only to check that it exists
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s links to %s, which does not exist", rel, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSpecs_ConstitutionCopiesMatch(t *testing.T) {
	root := moduleRoot(t)
	strip := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p)) //nolint:gosec // a fixed path under this repo's own checkout
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.HasPrefix(strings.TrimSpace(s), "<!--") {
			if i := strings.Index(s, "-->"); i >= 0 {
				s = s[i+3:]
			}
		}
		return strings.TrimSpace(s)
	}
	if strip(".specify/memory/constitution.md") != strip("docs/constitution.md") {
		t.Error(".specify/memory/constitution.md and docs/constitution.md differ beyond docs/'s amendment-history comment: edit both (CLAUDE.md)")
	}
}

func uniq(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func TestBPIDs(t *testing.T) {
	got := bpIDs("- [X] T1 x (bp 9.1, 9.2, 9.9; FR-111)\n- [X] T2 y (bp 14.1)\n- [X] T3 z (post-ledger)")
	want := []string{"9.1", "9.2", "9.9", "14.1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bpIDs = %v, want %v", got, want)
	}
}

func TestResolves(t *testing.T) {
	known := map[string]bool{"14.1": true, "6.9": true}
	aliases := map[string]string{"13.8": "14.1", "13.99": "99.9"}
	for id, want := range map[string]bool{"6.9": true, "13.8": true, "13.99": false, "7.7": false} {
		if got := resolves(id, known, aliases); got != want {
			t.Errorf("resolves(%s) = %v, want %v", id, got, want)
		}
	}
}

func TestPathTokens(t *testing.T) {
	line := "- [X] T1 in `internal/a/b.go`, `/healthz`, `platform/delegate`, `.dev/skills/`, `internal/c/`, `a/b@v1`, `docs/x.md#frag`, `text/event-stream`, `internal/*/x.go`"
	got := pathTokens(line)
	want := []string{"internal/a/b.go", "internal/c/", "docs/x.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pathTokens = %v, want %v", got, want)
	}
}
