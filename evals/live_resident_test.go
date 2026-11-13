package evals

import (
	"context"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
)

// The live track existed to show the model what production shows it, but its tools
// were toys with plain names, so it never sent a name like `platform/shell@v1`, which
// the provider API refuses on the wire. The resident cases use the shipped descriptors;
// this keeps them from drifting away from the real ones.
func TestLiveTaskCorpus_ResidentSchemasAreTheShippedDescriptors(t *testing.T) {
	for name, tc := range map[string]struct {
		corpus  provider.ToolSchema
		shipped tools.Descriptor
	}{
		"file_read":   {residentFileReadTool, builtin.FileRead{}.Descriptor()},
		"file_search": {residentFileSearchTool, builtin.FileSearch{}.Descriptor()},
		"shell":       {residentShellTool, builtin.Shell{}.Descriptor()},
	} {
		if tc.corpus.Name != tc.shipped.ID.String() {
			t.Errorf("%s: corpus name %q, shipped %q", name, tc.corpus.Name, tc.shipped.ID.String())
		}
		if tc.corpus.Description != tc.shipped.Description {
			t.Errorf("%s: corpus description %q, shipped %q", name, tc.corpus.Description, tc.shipped.Description)
		}
		if string(tc.corpus.InputSchema) != string(tc.shipped.InputSchema) {
			t.Errorf("%s: corpus schema %s, shipped %s", name, tc.corpus.InputSchema, tc.shipped.InputSchema)
		}
	}
}

// At least two cases use the resident catalog, and each one's tool list can be sent to a
// provider: the names translate to wire-safe ones without colliding.
func TestLiveTaskCorpus_ResidentCasesExistAndTheirToolNamesCanGoOnTheWire(t *testing.T) {
	resident := 0
	for _, c := range LiveTaskCorpus() {
		uses := false
		for _, tool := range c.Tools {
			if strings.HasPrefix(tool.Name, "platform/") {
				uses = true
			}
		}
		if !uses {
			continue
		}
		resident++
		names, err := provider.NewWireNames(c.Tools)
		if err != nil {
			t.Errorf("%s: tool names collide on the wire: %v", c.ID, err)
			continue
		}
		for _, tool := range c.Tools {
			if wire := names.Wire(tool.Name); strings.ContainsAny(wire, "/@.") || names.Ref(wire) != tool.Name {
				t.Errorf("%s: %q does not survive the translation (wire %q)", c.ID, tool.Name, wire)
			}
		}
		for _, call := range c.Expected.ToolCalls {
			if !strings.HasPrefix(call, "platform/") {
				t.Errorf("%s expects %q: a resident case expects the internal reference", c.ID, call)
			}
		}
	}
	if resident < 2 {
		t.Fatalf("%d live cases use the resident catalog, want at least 2", resident)
	}
}

// The harness runs a resident case end to end against the scripted provider, so a typo in
// one is caught here and not only under -tags=liveeval.
func TestLiveTaskCorpus_EachResidentCasePassesAScriptedRightAnswer(t *testing.T) {
	for _, c := range LiveTaskCorpus() {
		if !strings.HasPrefix(c.ID, "live-capability-resident-") {
			continue
		}
		var scripts []fake.Script
		for i, call := range c.Expected.ToolCalls {
			scripts = append(scripts, fake.Script{Chunks: []fake.ChunkSpec{
				{Kind: "tool_use", ToolUseID: "tu" + string(rune('1'+i)), ToolName: call, Input: `{}`},
				{Kind: "done", Done: "stop"},
			}})
		}
		scripts = append(scripts, fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "Done."}, {Kind: "done", Done: "stop"}}})

		trial := RunLiveTaskCase(context.Background(), fake.New(scripts...), "test-model", nil, c)
		if trial.Verdict != VerdictPass {
			t.Errorf("%s: a scripted right answer = %+v, want a pass", c.ID, trial)
		}
	}
}
