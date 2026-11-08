package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// tableOwners says which feature owns each table: the one package tree whose SQL
// may name it. It is the data half of the module rules (modules_test.go is the
// code half): a feature's tables are reached only through the feature's own
// package, so extracting the feature later moves its tables with it instead of
// untangling queries scattered across the codebase.
//
// A migration that adds a table fails TestTableOwnership_EveryTableHasAnOwner
// until the table is listed here, which is the point: a table with no owner is
// how the claims ledger, the checkpoint writer and the tenants insert once ended
// up in package main and the generic store.
var tableOwners = map[string]string{
	"approvals":             "internal/oversight",
	"audit_anchors":         "internal/audit",
	"audit_receipts":        "internal/audit",
	"board_cards":           "internal/teams",
	"budget_decisions":      "internal/cost",
	"budgets":               "internal/cost",
	"checkpoints":           "internal/runctl",
	"claims":                "internal/runctl",
	"content_access_grants": "internal/contentaccess",
	"cost_records":          "internal/cost",
	"cron_schedules":        "internal/surfaces/cron",
	"delegations":           "internal/delegate",
	"deliveries":            "internal/surfaces",
	"derived_artifacts":     "internal/tools",
	"documents":             "internal/retrieval",
	"encryption_keys":       "internal/crypto",
	"events":                "internal/eventlog",
	"fanout_envelopes":      "internal/delegate",
	"inbound_deliveries":    "internal/surfaces",
	"input_requests":        "internal/oversight",
	"mcp_servers":           "internal/surfaces/mcp",
	"messaging_channels":    "internal/surfaces",
	"oauth_connections":     "internal/connectors",
	"orchestration_plans":   "internal/plan",
	"price_book":            "internal/cost",
	"retrieval_chunks":      "internal/retrieval",
	"sandboxes":             "internal/sandbox",
	"session_lineage":       "internal/delegate",
	"sessions":              "internal/eventlog",
	"snapshots":             "internal/eventlog",
	"team_envelopes":        "internal/teams",
	"team_members":          "internal/teams",
	"teams":                 "internal/teams",
	"tenant_configs":        "internal/config",
	"tenants":               "internal/tenancy",
}

// kernelTables are the session/event aggregate every feature table references by
// session_id. The aggregate is the shared kernel, so unlike every other table it
// has several accessors; each is named here with the reason, and a new one fails
// until it is added in the diff. The persistence kit (internal/store) is the
// aggregate's own storage.
var kernelTables = map[string]map[string]string{
	"sessions": {
		"internal/store":     "the aggregate's own storage: create, read, list and set the status of a session",
		"internal/runctl":    "cancel, steer, resume, fork and the idle sweep move a session between statuses, and orphan recovery lists the running ones",
		"internal/delegate":  "a delegation resolves the parent session it hangs off",
		"internal/teams":     "a team's roster is the sessions whose team_id names it",
		"internal/tenancy":   "the operator's lookup of which tenant owns a session",
		"internal/reporting": "the read model",
	},
	"events": {
		"internal/store":     "the aggregate's own storage: append and read the log",
		"internal/eventlog":  "sealing under the session's own key reads the log for the key in use",
		"internal/audit":     "the audit chain is verified against the log it covers",
		"internal/crypto":    "erasure finds the keys and sessions a shred covers, and appends the erasure record",
		"internal/reporting": "the read model",
	},
}

// readModels may read (never write) any table: their job is to join what the
// features own into a report. The write rule still applies to them.
var readModels = []string{"internal/reporting"}

// tableRef is one table a SQL literal names and whether the statement writes it.
var tableRef = regexp.MustCompile(`(?i)\b(delete\s+from|insert\s+into|update|from|join)\s+([a-z_][a-z0-9_]*)`)

// createdTable and droppedTable read the schema a migration changes.
var (
	createdTable = regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	droppedTable = regexp.MustCompile(`(?im)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
)

// liveTables is every table the migrations leave in place.
func liveTables(t *testing.T, root string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found under %s/migrations (err=%v)", root, err)
	}
	sort.Strings(files) // applied in filename order
	live := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // path built from this repo's own migrations listing
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createdTable.FindAllStringSubmatch(string(b), -1) {
			live[strings.ToLower(m[1])] = true
		}
		for _, m := range droppedTable.FindAllStringSubmatch(string(b), -1) {
			delete(live, strings.ToLower(m[1]))
		}
	}
	return live
}

func TestTableOwnership_EveryTableHasAnOwner(t *testing.T) {
	root := moduleRoot(t)
	live := liveTables(t, root)

	for table := range live {
		if table == "schema_migrations" {
			continue
		}
		if tableOwners[table] == "" {
			t.Errorf("table %q is created by a migration but has no owner in tableOwners (tests/contract/ownership_test.go): name the feature whose package tree may write its SQL", table)
		}
	}
	for table, owner := range tableOwners {
		if !live[table] {
			t.Errorf("tableOwners lists %q, which no migration leaves in place: remove the stale entry", table)
		}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(owner))); err != nil || !info.IsDir() {
			t.Errorf("table %q is owned by %q, which is not a directory", table, owner)
		}
	}
	for table, accessors := range kernelTables {
		if tableOwners[table] == "" {
			t.Errorf("kernelTables names %q, which has no owner", table)
		}
		for dir := range accessors {
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err != nil || !info.IsDir() {
				t.Errorf("kernelTables[%q] names accessor %q, which is not a directory", table, dir)
			}
		}
	}
}

// TestTableOwnership_SQLStaysInTheOwner fails when a package's SQL names a table
// that belongs to another feature. The remedy is never to widen the lists: add
// the query to the owner and have the other feature call it through a port, or
// have cmd/nexusd inject the owner's writer when two must share a transaction
// (the way the budget writer reaches runs and the artifact store reaches erasure).
func TestTableOwnership_SQLStaysInTheOwner(t *testing.T) {
	root := moduleRoot(t)
	live := liveTables(t, root)

	var offenders []string
	checked := 0
	for _, top := range []string{"internal", "kernel", "cmd", "evals"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, rerr := os.ReadFile(path) //nolint:gosec // walking this repo's own source tree
			if rerr != nil {
				t.Fatal(rerr)
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			dir := rel[:strings.LastIndex(rel, "/")]
			checked++

			for _, lit := range stringLiteral.FindAllString(string(b), -1) {
				if sqlStatement.FindString(lit) == "" {
					continue
				}
				for _, m := range tableRef.FindAllStringSubmatch(lit, -1) {
					table := strings.ToLower(m[2])
					if !live[table] {
						continue
					}
					writes := !strings.EqualFold(m[1], "from") && !strings.EqualFold(m[1], "join")
					if reason := tableAccessViolation(dir, table, writes); reason != "" {
						offenders = append(offenders, rel+": "+reason)
					}
				}
			}
			return nil
		})
	}
	if checked < 100 {
		t.Fatalf("scanned only %d source files; the rule would pass vacuously", checked)
	}
	sort.Strings(offenders)
	offenders = dedupe(offenders)
	for _, o := range offenders {
		t.Error(o)
	}
}

// tableAccessViolation explains why a package at dir may not name table, or
// returns "" when it may.
func tableAccessViolation(dir, table string, writes bool) string {
	owner := tableOwners[table]
	if within(dir, owner) {
		return ""
	}
	if accessors, kernel := kernelTables[table]; kernel {
		for a := range accessors {
			if within(dir, a) {
				return ""
			}
		}
		return "SQL names kernel table " + table + ", whose accessors are listed in kernelTables; add " + dir + " there with the reason, or reach the session through its port"
	}
	for _, rm := range readModels {
		if within(dir, rm) {
			if writes {
				return "the read model " + rm + " writes " + table + "; a read model only reads"
			}
			return ""
		}
	}
	return "SQL names table " + table + ", which " + owner + " owns; move the query into " + owner + " and call it through a port (or have cmd/nexusd inject the owner's writer)"
}

// within reports whether dir is root or beneath it, excluding any deeper owner's
// subtree that tableOwners names (so internal/surfaces/cron does not count as
// internal/surfaces).
func within(dir, root string) bool {
	if dir != root && !strings.HasPrefix(dir, root+"/") {
		return false
	}
	for _, other := range tableOwners {
		if other != root && strings.HasPrefix(other, root+"/") && (dir == other || strings.HasPrefix(dir, other+"/")) {
			return false
		}
	}
	return true
}

func dedupe(sorted []string) []string {
	var out []string
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// TestEventTaxonomy_OwnersAreRealPackages checks the owner each event type names
// (internal/eventlog/taxonomy.go) is a package that exists, so the table cannot
// credit an event to a feature that was renamed or never was.
func TestEventTaxonomy_OwnersAreRealPackages(t *testing.T) {
	root := moduleRoot(t)
	types := eventlog.Types()
	if len(types) < 50 {
		t.Fatalf("taxonomy lists only %d event types; the rule would pass vacuously", len(types))
	}
	for _, typ := range types {
		owner := typ.Owner()
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(owner))); err != nil || !info.IsDir() {
			t.Errorf("event type %q is owned by %q, which is not a package directory", typ, owner)
		}
	}
}
