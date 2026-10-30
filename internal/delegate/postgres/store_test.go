package postgres

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestNullableJSON and TestNullIfEmpty are README task 13.8 (closing
// production-readiness finding F9): internal/delegate shipped with zero
// _test.go files despite owning delegation scope descent and taint ascend.
// These two are the pure helpers every DB-facing insert in this package
// goes through — small, but exactly the kind of thing worth pinning.
func TestNullableJSON(t *testing.T) {
	if got := nullableJSON(nil); got != nil {
		t.Errorf("nullableJSON(nil) = %v, want nil", got)
	}
	if got := nullableJSON(json.RawMessage{}); got != nil {
		t.Errorf("nullableJSON(empty) = %v, want nil", got)
	}
	in := json.RawMessage(`{"a":1}`)
	if got := nullableJSON(in); !bytes.Equal(got, in) {
		t.Errorf("nullableJSON(%s) = %s, want it returned verbatim", in, got)
	}
}

func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty(\"\") = %v, want nil", got)
	}
	got := nullIfEmpty("agent-1")
	if got == nil || *got != "agent-1" {
		t.Errorf("nullIfEmpty(\"agent-1\") = %v, want a pointer to \"agent-1\"", got)
	}
}
