package store

import (
	"encoding/json"
	"testing"
)

func TestUpcastContentV1ToV2(t *testing.T) {
	v1 := json.RawMessage(`{"text":"hi"}`)

	got, version, err := Upcast(EventContent, 1, v1)
	if err != nil {
		t.Fatalf("Upcast: %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("version = %d, want %d (the chain must reach the current version)", version, CurrentSchemaVersion)
	}

	var v2 contentV2
	if err := json.Unmarshal(got, &v2); err != nil {
		t.Fatalf("unmarshal upcast result: %v", err)
	}
	if v2.Body != "hi" {
		t.Fatalf("Body = %q, want %q", v2.Body, "hi")
	}
}

func TestUpcastNoRegisteredUpcasterIsAPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"anything":"goes"}`)

	got, version, err := Upcast(EventToolUse, 1, raw)
	if err != nil {
		t.Fatalf("Upcast: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want unchanged 1", version)
	}
	if string(got) != string(raw) {
		t.Fatalf("payload changed with no registered upcaster: got %s", got)
	}
}
