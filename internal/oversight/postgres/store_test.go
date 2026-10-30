package postgres

import "testing"

func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty(\"\") = %v, want nil", got)
	}
	got := nullIfEmpty("operator@example.com")
	if got == nil || *got != "operator@example.com" {
		t.Errorf("nullIfEmpty(...) = %v, want a pointer to the input", got)
	}
}
