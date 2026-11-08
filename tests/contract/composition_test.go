package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sqlStatement matches a SQL statement inside a string literal: the verb and
// the clause that makes it a statement rather than prose that happens to start
// with "select".
var sqlStatement = regexp.MustCompile(`(?is)\b(select\b.+?\bfrom\s+\w+|insert\s+into\s+\w+|update\s+\w+\s+set\b|delete\s+from\s+\w+)`)

// stringLiteral matches a Go raw or interpreted string literal.
var stringLiteral = regexp.MustCompile("(?s)`[^`]*`|\"(?:[^\"\\\\\\n]|\\\\.)*\"")

// TestComposition_MainContainsNoSQL pins the rule that cmd/nexusd is the
// composition root: it wires adapters to ports and decides when a scheduled pass
// runs, and it holds no SQL. A query in main is a use case or an adapter that
// has no package of its own, which is how a table ends up owned by nobody (it is
// also how the checkpoint writer, the idle sweep, the orphan finder, the
// messaging-channel adapter and the tenants insert once lived in package main).
//
// There is no exemption. The operator commands (cli_*.go: seed, erase, go-live)
// used to be one, on the grounds that a human runs them once with administrative
// credentials; each of the four statements they held belonged to a feature, and
// is now that feature's port (tenancy.Register, tenancy.SessionLocator,
// costpg.GetBudget), so a command is as thin as the server.
func TestComposition_MainContainsNoSQL(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "cmd", "nexusd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var offenders []string
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // path built from this repo's own cmd/nexusd listing
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, lit := range stringLiteral.FindAllString(string(b), -1) {
			if m := sqlStatement.FindString(lit); m != "" {
				offenders = append(offenders, name+": "+oneLine(m))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no source files found under cmd/nexusd; the rule would pass vacuously")
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("cmd/nexusd/%s holds SQL; move it behind a port into the package that owns the table", o)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 70 {
		s = s[:70] + "…"
	}
	return s
}
