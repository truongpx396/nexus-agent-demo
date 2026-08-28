package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExporterEmitFiltersBeforeWriting(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf)

	if err := exp.Emit("turn", Attrs{
		"session.id": "s-1",
		"content":    "this must never appear in the exported line",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	line := buf.String()
	if strings.Contains(line, "this must never appear") {
		t.Fatalf("exported line contains filtered content: %s", line)
	}

	var got Span
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal exported span: %v", err)
	}
	if got.Name != "turn" {
		t.Fatalf("Name = %q, want %q", got.Name, "turn")
	}
	if _, ok := got.Attrs["content"]; ok {
		t.Fatal("decoded span still carries the content key")
	}
	if got.Attrs["session.id"] != "s-1" {
		t.Fatalf("decoded span lost the allowlisted session.id: %v", got.Attrs)
	}
}
