package kernel_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

func openPayload(_ context.Context, e eventlog.Event) ([]byte, error) { return e.Payload, nil }

var mcpTool = provider.ToolSchema{
	Name:        "mcp/github/list_prs@v1",
	Description: "list pull requests",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`),
}

// freshConfig is what a surface hands Seed for a run with tenant memory and one
// per-user MCP tool, on top of one resident tool.
func freshConfig() kernel.RunConfig {
	return kernel.RunConfig{
		System: "base system", ModelID: "fake", MaxTurns: 5, Input: "go", AutonomyLevel: "supervised",
		Catalog:       []provider.ToolSchema{{Name: "platform/echo@v1"}},
		LoadedTools:   []string{"platform/echo@v1", mcpTool.Name},
		MemorySources: []string{"notes.md"},
		MemoryText:    "the tenant prefers metric units",
		ExtraCatalog:  []provider.ToolSchema{mcpTool},
	}
}

func seeded(t *testing.T, cfg kernel.RunConfig) *rig {
	t.Helper()
	r := newRig(toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult { return kernel.ToolResult{} }), finalTurn)
	for _, err := range r.k.Seed(context.Background(), r.st, cfg) {
		if err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestRehydratePinned_ReadsTheMemoryAndTheRunsToolsBackFromTheLog(t *testing.T) {
	r := seeded(t, freshConfig())

	got, err := kernel.RehydratePinned(context.Background(), r.log.Events(r.sessionID), openPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryText != "the tenant prefers metric units" {
		t.Errorf("MemoryText = %q", got.MemoryText)
	}
	if len(got.ExtraCatalog) != 1 {
		t.Fatalf("ExtraCatalog = %+v, want only the MCP tool (the resident one is known to every process)", got.ExtraCatalog)
	}
	tool := got.ExtraCatalog[0]
	if tool.Name != mcpTool.Name || tool.Description != mcpTool.Description || string(tool.InputSchema) != string(mcpTool.InputSchema) {
		t.Errorf("ExtraCatalog[0] = %+v, want %+v", tool, mcpTool)
	}
}

func TestRehydratePinned_ASessionWithoutThemIsTheZeroValue(t *testing.T) {
	// A session started before these payloads carried text and schemas: a
	// memory_loaded with sources only and an id-only tool_loaded.
	cfg := freshConfig()
	cfg.MemoryText, cfg.ExtraCatalog = "", nil
	cfg.LoadedTools = []string{"platform/echo@v1"}
	r := seeded(t, cfg)

	got, err := kernel.RehydratePinned(context.Background(), r.log.Events(r.sessionID), openPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryText != "" || got.ExtraCatalog != nil {
		t.Fatalf("RehydratePinned = %+v, want the zero value", got)
	}
	if empty, err := kernel.RehydratePinned(context.Background(), nil, openPayload); err != nil || empty.MemoryText != "" || empty.ExtraCatalog != nil {
		t.Fatalf("RehydratePinned(nil history) = %+v, %v", empty, err)
	}
}

// Only memory_loaded and tool_loaded are opened: the rest of a long history is
// sealed content that has no business being decrypted here.
func TestRehydratePinned_OpensOnlyTheEventsItNeeds(t *testing.T) {
	r := seeded(t, freshConfig())
	opened := map[eventlog.EventType]int{}
	_, err := kernel.RehydratePinned(context.Background(), r.log.Events(r.sessionID), func(ctx context.Context, e eventlog.Event) ([]byte, error) {
		opened[e.Type]++
		return openPayload(ctx, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	for typ := range opened {
		if typ != eventlog.EventMemoryLoaded && typ != eventlog.EventToolLoaded {
			t.Errorf("RehydratePinned decrypted a %s event", typ)
		}
	}
	if opened[eventlog.EventUserMessage] != 0 {
		t.Error("the user's message was decrypted")
	}
}

// base.Catalog is the process-wide slice every concurrent run shares: appending
// into spare capacity would let two runs write the same array.
func TestPinned_ApplyNeverWritesThroughTheCallersCatalog(t *testing.T) {
	shared := make([]provider.ToolSchema, 1, 8)
	shared[0] = provider.ToolSchema{Name: "platform/echo@v1"}
	base := kernel.RunConfig{System: "base", Catalog: shared}

	got := kernel.Pinned{MemoryText: "mem", ExtraCatalog: []provider.ToolSchema{mcpTool}}.Apply(base)

	if got.System != "base\n\nmem" {
		t.Errorf("System = %q", got.System)
	}
	if len(got.Catalog) != 2 || got.Catalog[1].Name != mcpTool.Name {
		t.Fatalf("Catalog = %+v", got.Catalog)
	}
	for _, spare := range shared[1:cap(shared)] {
		if spare.Name != "" {
			t.Errorf("Apply wrote %q into the shared catalog's spare capacity", spare.Name)
		}
	}
	if len(base.Catalog) != 1 || base.System != "base" {
		t.Error("Apply modified its argument")
	}
	if same := (kernel.Pinned{}).Apply(base); same.System != "base" || len(same.Catalog) != 1 {
		t.Errorf("the zero Pinned changed the config: %+v", same)
	}
}

// streamRecorder remembers the system prompt and tool list of every call.
type streamRecorder struct {
	inner   provider.Provider
	systems []string
	tools   [][]provider.ToolSchema
}

func (s *streamRecorder) Stream(ctx context.Context, pr provider.Prompt, tools []provider.ToolSchema, rc provider.RunContext) (provider.Stream, error) {
	s.systems = append(s.systems, pr.System)
	s.tools = append(s.tools, slices.Clone(tools))
	return s.inner.Stream(ctx, pr, tools, rc)
}

func toolNames(tools []provider.ToolSchema) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

// The finding itself: the process that starts a run only seeds it, and a worker
// drives the turns from a config it builds with no knowledge of that run's
// memory or MCP tools. The worker's prompt must still carry them, read from the
// log.
func TestContinue_AWorkerShowsTheModelTheMemoryAndToolsTheSessionStartedWith(t *testing.T) {
	r := seeded(t, freshConfig())
	rec := &streamRecorder{inner: fake.New(finalTurn)}
	r.k.Provider = rec

	// The worker rebuilds the state from the log, as runstate.Load does, and its
	// config is only what every process holds.
	history := r.log.Events(r.sessionID)
	transcript, ids, err := kernel.Rehydrate(context.Background(), history, openPayload)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := kernel.RehydratePinned(context.Background(), history, openPayload)
	if err != nil {
		t.Fatal(err)
	}
	st := &kernel.RunState{TenantID: r.tenantID, SessionID: r.sessionID, Seal: seal, History: history, Transcript: transcript, ToolUseIDs: ids, Pinned: pinned}
	workerCfg := kernel.RunConfig{
		System: "base system", ModelID: "fake", MaxTurns: 5, AutonomyLevel: "supervised",
		Catalog: []provider.ToolSchema{{Name: "platform/echo@v1"}},
	}

	for _, err := range r.k.Continue(context.Background(), st, workerCfg) {
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(rec.systems) != 1 {
		t.Fatalf("the provider was called %d times, want 1", len(rec.systems))
	}
	if want := "base system\n\nthe tenant prefers metric units"; rec.systems[0] != want {
		t.Errorf("system prompt = %q, want %q", rec.systems[0], want)
	}
	if got := toolNames(rec.tools[0]); !slices.Equal(got, []string{"platform/echo@v1", mcpTool.Name}) {
		t.Errorf("tools = %v, want the resident tool then the MCP tool", got)
	}
	if string(rec.tools[0][1].InputSchema) != string(mcpTool.InputSchema) {
		t.Errorf("the MCP tool's schema did not survive the log: %s", rec.tools[0][1].InputSchema)
	}

	// Without the pinned values, which is what every resuming service passed
	// before, the model sees neither.
	bare := &kernel.RunState{TenantID: r.tenantID, SessionID: r.sessionID, Seal: seal, History: history, Transcript: transcript, ToolUseIDs: ids}
	rec2 := &streamRecorder{inner: fake.New(finalTurn)}
	r.k.Provider = rec2
	for _, err := range r.k.Continue(context.Background(), bare, workerCfg) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(rec2.systems[0], "metric units") || len(rec2.tools[0]) != 1 {
		t.Fatalf("a state with no Pinned should be today's behaviour, got system %q and tools %v", rec2.systems[0], toolNames(rec2.tools[0]))
	}
}

// Run seeds and drives the loop in one call, so it has to show the model the
// same thing a later resume will read back from the log.
func TestRun_ShowsTheModelTheMemoryAndToolsItRecorded(t *testing.T) {
	r := newRig(toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult { return kernel.ToolResult{} }), finalTurn)
	rec := &streamRecorder{inner: fake.New(finalTurn)}
	r.k.Provider = rec

	for _, err := range r.k.Run(context.Background(), r.st, freshConfig()) {
		if err != nil {
			t.Fatal(err)
		}
	}

	if want := "base system\n\nthe tenant prefers metric units"; rec.systems[0] != want {
		t.Errorf("system prompt = %q, want %q", rec.systems[0], want)
	}
	if got := toolNames(rec.tools[0]); !slices.Equal(got, []string{"platform/echo@v1", mcpTool.Name}) {
		t.Errorf("tools = %v", got)
	}
	// What Run showed is what the log says.
	pinned, err := kernel.RehydratePinned(context.Background(), r.log.Events(r.sessionID), openPayload)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.MemoryText != "the tenant prefers metric units" || len(pinned.ExtraCatalog) != 1 {
		t.Fatalf("the log disagrees with what Run showed: %+v", pinned)
	}
}
