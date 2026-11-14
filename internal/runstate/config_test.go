package runstate_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/provider"
	"github.com/truongpx396/nexus-agent/internal/runstate"
	"github.com/truongpx396/nexus-agent/kernel"
)

func TestConfigFor_TakesPinnedFieldsFromTheSessionAndLeavesTheRestAlone(t *testing.T) {
	sess := eventlog.Session{
		SessionID: uuid.New(), AutonomyLevel: "read_only", RouteModelID: "routed", Conversational: true,
	}
	catalog := []provider.ToolSchema{{Name: "platform/x@v1"}}
	// base deliberately disagrees with the session on every pinned field: a resume
	// continues the session's run, so the session's value must win.
	base := kernel.RunConfig{
		System: "sys", Catalog: catalog, MaxTurns: 7,
		AutonomyLevel: "autonomous", ModelID: "other", Conversational: false,
	}

	got := runstate.ConfigFor(sess, base)

	if got.AutonomyLevel != "read_only" || got.ModelID != "routed" || !got.Conversational {
		t.Errorf("pinned fields = autonomy %q model %q conversational %v, want read_only routed true",
			got.AutonomyLevel, got.ModelID, got.Conversational)
	}
	if got.System != "sys" || got.MaxTurns != 7 || len(got.Catalog) != 1 || got.Catalog[0].Name != "platform/x@v1" {
		t.Errorf("service-held fields changed: %+v", got)
	}
	if base.AutonomyLevel != "autonomous" || base.ModelID != "other" {
		t.Error("ConfigFor modified its argument; it must return a new config")
	}
}

// resumeClass says where a kernel.RunConfig field comes from when a run is resumed.
type resumeClass int

const (
	// pinned: the session fixed it at creation, so a resume reads it from the
	// session row. runstate.ConfigFor sets it.
	pinned resumeClass = iota + 1
	// serviceHeld: the resuming service holds one value for every session it
	// resumes (the process-wide system prompt, catalog and turn cap).
	serviceHeld
	// freshOnly: only a run's first call reads it (the opening message, the tools
	// and memory folded into the first prompt), so a resume has nothing to restore.
	freshOnly
)

// runConfigFields classifies every field of kernel.RunConfig. Adding a field to
// RunConfig fails TestRunConfigFieldsAreAllClassified until it is listed here,
// which is the moment to decide how a resumed run gets it: if the answer is
// "from the session", it belongs in pinned and in ConfigFor, and the test below
// checks ConfigFor actually sets it. The omission this prevents is real: three of
// the four resuming services once dropped Conversational, so a chat that needed
// an approval ended instead of waiting for the next message.
var runConfigFields = map[string]resumeClass{
	"AutonomyLevel":    pinned,
	"ModelID":          pinned,
	"Conversational":   pinned,
	"System":           serviceHeld,
	"Catalog":          serviceHeld,
	"MaxTurns":         serviceHeld,
	"CondenserModelID": serviceHeld,
	"Input":            freshOnly,
	"LoadedTools":      freshOnly,
	"MemorySources":    freshOnly,
	"MemoryText":       freshOnly,
	"ExtraCatalog":     freshOnly,
}

func TestRunConfigFieldsAreAllClassified(t *testing.T) {
	typ := reflect.TypeFor[kernel.RunConfig]()
	seen := map[string]bool{}
	var unclassified []string
	for field := range typ.Fields() {
		name := field.Name
		seen[name] = true
		if _, ok := runConfigFields[name]; !ok {
			unclassified = append(unclassified, name)
		}
	}
	var stale []string
	for name := range runConfigFields {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	slices.Sort(unclassified)
	slices.Sort(stale)
	if len(unclassified) > 0 {
		t.Errorf("kernel.RunConfig has fields with no resume class: %v; add each to runConfigFields, and to runstate.ConfigFor if the session pins it", unclassified)
	}
	if len(stale) > 0 {
		t.Errorf("runConfigFields names fields kernel.RunConfig no longer has: %v", stale)
	}
}

// TestConfigForSetsExactlyThePinnedFields holds ConfigFor to the classification
// above: it overwrites every pinned field and touches nothing else.
func TestConfigForSetsExactlyThePinnedFields(t *testing.T) {
	base := populated(t, reflect.TypeFor[kernel.RunConfig]())
	got := runstate.ConfigFor(eventlog.Session{}, base.Interface().(kernel.RunConfig))

	gv := reflect.ValueOf(got)
	for i := range gv.NumField() {
		name := gv.Type().Field(i).Name
		changed := !reflect.DeepEqual(gv.Field(i).Interface(), base.Field(i).Interface())
		switch class := runConfigFields[name]; {
		case class == pinned && !changed:
			t.Errorf("%s is classified pinned but ConfigFor left it as the caller had it", name)
		case class != pinned && changed:
			t.Errorf("%s is not pinned but ConfigFor changed it", name)
		}
	}
}

// populated returns a value of typ with every field set to something non-zero,
// so that "ConfigFor overwrote it" is distinguishable from "it was already zero".
func populated(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	v := reflect.New(typ).Elem()
	for i := range typ.NumField() {
		switch p := v.Field(i).Addr().Interface().(type) {
		case *string:
			*p = "set"
		case *int:
			*p = 7
		case *bool:
			*p = true
		case *[]string:
			*p = []string{"set"}
		case *[]provider.ToolSchema:
			*p = []provider.ToolSchema{{Name: "set"}}
		default:
			t.Fatalf("RunConfig.%s has type %s, which populated does not handle; teach it", typ.Field(i).Name, v.Field(i).Type())
		}
	}
	return v
}
