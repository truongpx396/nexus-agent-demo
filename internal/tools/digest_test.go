package tools

import (
	"encoding/json"
	"testing"
)

func TestCanonicalDigest_KeyOrderDoesNotMatter(t *testing.T) {
	a, err := CanonicalDigest("platform/shell@v1", json.RawMessage(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	b, err := CanonicalDigest("platform/shell@v1", json.RawMessage(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("digests differ for the same object with keys in a different order")
	}
}

func TestCanonicalDigest_WhitespaceDoesNotMatter(t *testing.T) {
	a, err := CanonicalDigest("x/y@v1", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	b, err := CanonicalDigest("x/y@v1", json.RawMessage("{\n  \"a\" : 1\n}\n"))
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("digests differ for the same object with different insignificant whitespace")
	}
}

func TestCanonicalDigest_DifferentInputDiffers(t *testing.T) {
	a, _ := CanonicalDigest("x/y@v1", json.RawMessage(`{"a":1}`))
	b, _ := CanonicalDigest("x/y@v1", json.RawMessage(`{"a":2}`))
	if string(a) == string(b) {
		t.Fatal("digests match for different input")
	}
}

func TestCanonicalDigest_DifferentToolIDDiffers(t *testing.T) {
	a, _ := CanonicalDigest("x/y@v1", json.RawMessage(`{"a":1}`))
	b, _ := CanonicalDigest("x/z@v1", json.RawMessage(`{"a":1}`))
	if string(a) == string(b) {
		t.Fatal("digests match for the same input under different tool_ids")
	}
}

func TestCanonicalDigest_NestedObjectsAndArrays(t *testing.T) {
	a, err := CanonicalDigest("x/y@v1", json.RawMessage(`{"outer":{"z":1,"a":[3,2,1]},"list":["b","a"]}`))
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	b, err := CanonicalDigest("x/y@v1", json.RawMessage(`{"list":["b","a"],"outer":{"a":[3,2,1],"z":1}}`))
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("digests differ for the same nested structure with top-level keys reordered (array element order must NOT be sorted)")
	}
}

func TestCanonicalDigest_EmptyInputTreatedAsEmptyObject(t *testing.T) {
	a, err := CanonicalDigest("x/y@v1", nil)
	if err != nil {
		t.Fatalf("CanonicalDigest(nil): %v", err)
	}
	b, err := CanonicalDigest("x/y@v1", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CanonicalDigest({}): %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("nil input should digest identically to an explicit empty object")
	}
}
