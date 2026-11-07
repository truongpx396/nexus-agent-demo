package contract

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// This file holds the two rules that make the codebase a modular monolith and
// not merely a monolith with packages in it. boundaries_test.go says what the
// core must not reach (a database, an HTTP client, a child process); these say
// who may depend on whom *between* features.
//
//  1. An adapter is a feature's own. internal/plan/postgres may use plan, the
//     domain, and the shared persistence kit, but not internal/delegate/postgres:
//     a feature talks to another feature through its package, and the
//     composition root injects whatever has to happen in the same transaction.
//
//  2. Feature-to-feature imports are declared. Each package below lists the
//     other packages it may import; a new edge fails the build until it is added
//     here, which puts the coupling decision in the diff where a reviewer sees it.
//     The table starts as the graph the code actually had when the rule landed,
//     so it ratchets: it can only grow by being edited on purpose.

// adapterDirs are the last path elements that mark a driven adapter, a
// transport binding or a test kit: code that touches the outside world or
// stands in for it. Everything else under internal/ (and kernel/) is a feature
// or a piece of the core.
var adapterDirs = map[string]bool{
	"postgres": true, "redis": true, "inmem": true, "sealed": true,
	"otlp": true, "langfuse": true, "profiling": true, "exechook": true, "httphook": true,
	"kerneladapter": true, "anthropic": true, "litellm": true, "fake": true, "wire": true,
	"signerkey": true, "costtest": true, "logtest": true, "oauth2": true, "resttest": true,
}

// persistenceKit is the shared way every feature's Postgres adapter reaches its
// tenant-scoped transaction, seals and receipts what it writes: the pool and
// tenant scoping (store), the audit chain, the key store and the sealed unit of
// work. Every feature's adapter may use it; it is one module, not a feature.
var persistenceKit = map[string]bool{
	modulePrefix + "internal/store":           true,
	modulePrefix + "internal/audit/postgres":  true,
	modulePrefix + "internal/crypto/postgres": true,
	modulePrefix + "internal/eventlog/sealed": true,
}

func isAdapter(pkgPath string) bool {
	rel := strings.TrimPrefix(pkgPath, modulePrefix)
	return adapterDirs[rel[strings.LastIndex(rel, "/")+1:]]
}

func loadModule(t *testing.T) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedImports}
	pkgs, err := packages.Load(cfg, modulePrefix+"internal/...", modulePrefix+"kernel")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if len(pkgs) < 40 {
		t.Fatalf("loaded %d packages; the module layout changed under this test", len(pkgs))
	}
	return pkgs
}

func TestAdapterIsolation(t *testing.T) {
	var violations []string
	for _, p := range loadModule(t) {
		if !isAdapter(p.PkgPath) || persistenceKit[p.PkgPath] {
			continue
		}
		// The feature an adapter belongs to is the package it sits under.
		own := p.PkgPath[:strings.LastIndex(p.PkgPath, "/")]
		for imp := range p.Imports {
			if !strings.HasPrefix(imp, modulePrefix) || !isAdapter(imp) || persistenceKit[imp] {
				continue
			}
			if imp == own || strings.HasPrefix(imp, own+"/") {
				continue
			}
			violations = append(violations, strings.TrimPrefix(p.PkgPath, modulePrefix)+" imports "+strings.TrimPrefix(imp, modulePrefix))
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("%s: an adapter may import its own feature, the domain and the shared persistence kit, never another feature's adapter; inject the dependency from cmd/nexusd instead", v)
	}
}

// allowedFeatureImports maps each feature or core package to the other
// non-adapter packages it may import. A package absent from the map may import
// none. Adding an entry is a design decision: say why in the PR.
var allowedFeatureImports = map[string][]string{
	"internal/connectors":         {"internal/config", "internal/crypto"},
	"internal/contentaccess":      {"internal/eventlog"},
	"internal/contentaccess/rest": {"internal/contentaccess", "internal/surfaces/rest"},
	"internal/cost":               {"internal/provider"},
	"internal/crypto":             {"internal/eventlog"},
	"internal/delegate":           {"internal/cost", "internal/crypto", "internal/eventlog", "internal/obs", "internal/provider", "internal/runstate", "kernel"},
	"internal/evalgate":           {"evals", "internal/plan"}, // the one bridge from the plan lifecycle to the release gate
	"internal/ingest":             {"internal/crypto", "internal/tools"},
	"internal/memory":             {"internal/config", "internal/tools"},
	"internal/oversight":          {"internal/crypto", "internal/eventlog", "internal/provider", "internal/tools", "kernel"},
	"internal/oversight/rest":     {"internal/oversight", "internal/surfaces/rest"},
	"internal/permissions":        {"internal/permissions/safety"},
	"internal/plan":               {"internal/cost", "internal/crypto", "internal/delegate", "internal/eventlog", "internal/oversight", "internal/provider", "internal/runstate", "kernel"},
	"internal/promptctx":          {"internal/provider"},
	"internal/queue":              {"internal/reliability"},
	"internal/reporting":          {"internal/obs"},
	"internal/retrieval":          {"internal/cost", "internal/ingest", "internal/memory", "internal/provider", "internal/tools"},
	"internal/runctl/rest":        {"internal/runctl", "internal/surfaces/rest"},
	"internal/runctl":             {"internal/crypto", "internal/eventlog", "internal/harness", "internal/oversight", "internal/permissions", "internal/provider", "internal/queue", "internal/runstate", "internal/tools", "kernel"},
	"internal/runs/rest":          {"internal/eventlog", "internal/obs", "internal/provider", "internal/runs", "internal/surfaces", "internal/surfaces/capability", "internal/surfaces/rest"},
	"internal/runs":               {"internal/controlplane", "internal/crypto", "internal/eventlog", "internal/harness", "internal/provider"},
	"internal/runstate":           {"internal/crypto", "internal/eventlog", "kernel"},
	"internal/skills":             {"internal/tools"},
	"internal/surfaces/cli":       {"internal/surfaces/capability", "internal/version"},
	"internal/surfaces/cron":      {"internal/harness", "internal/provider", "internal/runs", "internal/surfaces/capability"},
	"internal/surfaces/email":     {"internal/eventlog", "internal/runs", "internal/surfaces", "internal/surfaces/capability"},
	"internal/surfaces/mcp":       {"internal/provider", "internal/surfaces/capability", "internal/tools"},
	"internal/surfaces/rest":      {"internal/surfaces/capability"}, // the kit: no feature, so no endpoint
	"internal/surfaces/telegram":  {"internal/eventlog", "internal/runs", "internal/surfaces", "internal/surfaces/capability"},
	"internal/surfaces/web":       {"internal/surfaces/capability"},
	"internal/surfaces/zalo":      {"internal/eventlog", "internal/runs", "internal/surfaces", "internal/surfaces/capability"},
	"internal/teams":              {"internal/cost", "internal/crypto", "internal/eventlog", "internal/memory", "internal/provider", "internal/runstate", "kernel"},
	"internal/tools":              {"internal/hooks", "internal/permissions"},
	"internal/tools/builtin":      {"internal/skills", "internal/tools"},
	"kernel":                      {"internal/cost", "internal/eventlog", "internal/obs", "internal/promptctx", "internal/provider", "internal/reliability"},
}

func TestFeatureDependencies(t *testing.T) {
	var violations []string
	checked := 0
	for _, p := range loadModule(t) {
		if isAdapter(p.PkgPath) || persistenceKit[p.PkgPath] {
			continue
		}
		checked++
		rel := strings.TrimPrefix(p.PkgPath, modulePrefix)
		allowed := map[string]bool{}
		for _, a := range allowedFeatureImports[rel] {
			allowed[modulePrefix+a] = true
		}
		for imp := range p.Imports {
			if !strings.HasPrefix(imp, modulePrefix) || isAdapter(imp) || persistenceKit[imp] {
				continue
			}
			if !allowed[imp] {
				violations = append(violations, rel+" -> "+strings.TrimPrefix(imp, modulePrefix))
			}
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d packages; this rule would pass vacuously", checked)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("%s is not a declared dependency: if the coupling is intended, add it to allowedFeatureImports in tests/contract/modules_test.go and say why in the PR", v)
	}
}
