package eventlog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// declaredEventTypes reads event.go and returns the value of every constant it
// declares with type EventType, so the table cannot drift from the list.
func declaredEventTypes(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "event.go", nil, 0)
	if err != nil {
		t.Fatalf("parse event.go: %v", err)
	}
	out := map[string]string{} // value -> constant name
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EventType" {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				if prev, dup := out[val]; dup {
					t.Errorf("event type %q is declared twice (%s and %s)", val, prev, name.Name)
				}
				out[val] = name.Name
			}
		}
	}
	return out
}

func TestTaxonomy_EveryDeclaredTypeHasAnOwnerAndNothingIsStale(t *testing.T) {
	declared := declaredEventTypes(t)
	if len(declared) < 50 {
		t.Fatalf("found only %d declared event types; the parse is vacuous", len(declared))
	}
	for val, name := range declared {
		if EventType(val).Owner() == "" {
			t.Errorf("%s (%q) is declared in event.go but has no owner in taxonomy.go: say which feature defines it", name, val)
		}
	}
	var stale []string
	for typ := range owners {
		if _, ok := declared[string(typ)]; !ok {
			stale = append(stale, string(typ))
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("taxonomy.go owns %q, which event.go does not declare", s)
	}
}

func TestTaxonomy_KnownAndTypes(t *testing.T) {
	if !EventTerminal.Known() || EventTerminal.Owner() != "kernel" {
		t.Errorf("terminal: Known=%v Owner=%q, want a kernel-owned type", EventTerminal.Known(), EventTerminal.Owner())
	}
	if EventType("not_a_real_event").Known() || EventType("not_a_real_event").Owner() != "" {
		t.Error("a type outside the taxonomy must be unknown and ownerless")
	}
	types := Types()
	if len(types) != len(owners) {
		t.Fatalf("Types() returned %d, want %d", len(types), len(owners))
	}
	if !sort.SliceIsSorted(types, func(i, j int) bool { return types[i] < types[j] }) {
		t.Error("Types() is not sorted")
	}
}
