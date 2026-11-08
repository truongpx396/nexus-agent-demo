package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// resumingFeatures are the packages that continue a run from outside a live
// kernel.Run call: runctl after a crash or an idle pause, oversight after an
// approval, delegate when a child returns, plan between steps.
var resumingFeatures = []string{"internal/oversight", "internal/runctl", "internal/delegate", "internal/plan"}

// pinnedRunConfigFields are the kernel.RunConfig fields a session fixes at
// creation, which a resumed run must read from the session row
// (runstate.ConfigFor sets them; internal/runstate/config_test.go keeps this list
// and that function in step with the struct).
var pinnedRunConfigFields = map[string]bool{"AutonomyLevel": true, "ModelID": true, "Conversational": true}

// runConfigLiteralExempt lists the files in those packages that may still set a
// pinned field in a kernel.RunConfig literal, each with why.
var runConfigLiteralExempt = map[string]string{
	"internal/delegate/spawn.go": "builds the config of the child session it creates in the same call, from the parent it was just copied from; there is no stored row to read yet",
}

// TestResumingFeaturesDoNotPinRunConfigByHand pins the rule that a feature that
// resumes a run takes the fields its session pinned from runstate.ConfigFor. A
// literal may still carry what the service itself holds (System, Catalog,
// MaxTurns) as ConfigFor's base, but not AutonomyLevel, ModelID or Conversational.
// Each of the four used to set those by hand and each took a different subset, so
// a conversational chat resumed after an approval ended instead of waiting for the
// next message. One function means a new pinned field is added once.
func TestResumingFeaturesDoNotPinRunConfigByHand(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	exemptUsed := map[string]bool{}
	checked := 0

	for _, pkg := range resumingFeatures {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := pkg + "/" + name
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			checked++
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "RunConfig" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "kernel" {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || !pinnedRunConfigFields[key.Name] {
						continue
					}
					if _, ok := runConfigLiteralExempt[rel]; ok {
						exemptUsed[rel] = true
						continue
					}
					offenders = append(offenders, rel+" sets "+key.Name)
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("no source files found in the resuming packages; the rule would pass vacuously")
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s in a kernel.RunConfig literal: take the session's pinned fields from runstate.ConfigFor(session, base) so none is forgotten", o)
	}
	for rel, why := range runConfigLiteralExempt {
		if !exemptUsed[rel] {
			t.Errorf("%s is exempt (%s) but no longer sets a pinned field in a kernel.RunConfig literal; remove the entry", rel, why)
		}
	}
}
