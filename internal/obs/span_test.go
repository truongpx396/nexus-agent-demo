package obs

import (
	"bytes"
	"context"
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

	var got spanLine
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

func TestExporterStartSpanWritesOneLineAtEndWithKindAndDuration(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf)

	_, span := exp.StartSpan(context.Background(), "model.call", ObservationGeneration, Attrs{
		"model.id": "qwen2.5-local", "content": "this must never appear",
	})
	if buf.Len() != 0 {
		t.Fatalf("StartSpan wrote before End: %q", buf.String())
	}
	span.End(Attrs{"outcome": "stop"})

	var got spanLine
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal exported span: %v", err)
	}
	if got.Name != "model.call" || got.Kind != ObservationGeneration {
		t.Fatalf("Name/Kind = %q/%q, want %q/%q", got.Name, got.Kind, "model.call", ObservationGeneration)
	}
	if got.Attrs["model.id"] != "qwen2.5-local" || got.Attrs["outcome"] != "stop" {
		t.Fatalf("attrs from StartSpan and End not both present: %v", got.Attrs)
	}
	if _, ok := got.Attrs["content"]; ok {
		t.Fatal("decoded span still carries the content key")
	}
	if got.DurationMS < 0 {
		t.Fatalf("DurationMS = %d, want >= 0", got.DurationMS)
	}
}
