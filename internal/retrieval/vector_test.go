package retrieval

import "testing"

func TestVectorRoundTrip(t *testing.T) {
	v := []float32{0.5, -0.25, 1, -1, 0}
	s := formatVector(v)
	got, err := parseVector(s)
	if err != nil {
		t.Fatalf("parseVector: %v", err)
	}
	if len(got) != len(v) {
		t.Fatalf("length = %d, want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("component %d = %v, want %v", i, got[i], v[i])
		}
	}
}

func TestFormatVector_Shape(t *testing.T) {
	got := formatVector([]float32{1, 2, 3})
	want := "[1,2,3]"
	if got != want {
		t.Errorf("formatVector = %q, want %q", got, want)
	}
}

func TestParseVector_Empty(t *testing.T) {
	got, err := parseVector("[]")
	if err != nil {
		t.Fatalf("parseVector: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero components, got %d", len(got))
	}
}
