package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestConsolidate_ReserveFalseNeverCallsCondenser(t *testing.T) {
	s := &Store{RootDir: t.TempDir()}
	tenantID := uuid.New()
	if err := s.Write(tenantID, "notes.md", []byte("line one\nline two\nline three\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	called := false
	condenser := func(string) (string, error) { called = true; return "", nil }
	if err := s.Consolidate(tenantID, "notes.md", func() bool { return false }, condenser); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if called {
		t.Error("Consolidate called condenser even though reserve() returned false")
	}

	got, err := os.ReadFile(filepath.Join(s.RootDir, tenantID.String(), "notes.md"))
	if err != nil {
		t.Fatalf("read consolidated file: %v", err)
	}
	want := ExtractivePass("line one\nline two\nline three\n")
	if string(got) != want {
		t.Errorf("consolidated content = %q, want extractive pass %q", got, want)
	}
}

func TestConsolidate_CondenserErrorFallsBackToExtractive(t *testing.T) {
	s := &Store{RootDir: t.TempDir()}
	tenantID := uuid.New()
	original := "first\nsecond\nthird\nfourth\n"
	if err := s.Write(tenantID, "notes.md", []byte(original)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	condenser := func(string) (string, error) { return "", errors.New("model unavailable") }
	if err := s.Consolidate(tenantID, "notes.md", func() bool { return true }, condenser); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(s.RootDir, tenantID.String(), "notes.md"))
	if err != nil {
		t.Fatalf("read consolidated file: %v", err)
	}
	if string(got) != ExtractivePass(original) {
		t.Errorf("consolidated content = %q, want the extractive fallback", got)
	}
}

func TestConsolidate_DurableWritePrecedesDiscardingSource(t *testing.T) {
	// A failure writing the temp file (simulated by making the tenant
	// directory read-only after the source is written) must leave the
	// original source completely intact — task 7.2's literal ordering
	// invariant: the durable write of the replacement must succeed before
	// the source is ever at risk. Skipped when running as root, since a
	// root process ignores directory write permissions.
	if os.Geteuid() == 0 {
		t.Skip("no permission enforcement running as root")
	}

	s := &Store{RootDir: t.TempDir()}
	tenantID := uuid.New()
	original := "irreplaceable notes\n"
	if err := s.Write(tenantID, "notes.md", []byte(original)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	dir := filepath.Join(s.RootDir, tenantID.String())
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // test-only: deliberately read+execute-only so the temp file write fails
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) //nolint:errcheck,gosec // best-effort cleanup so t.TempDir() can remove it

	err := s.Consolidate(tenantID, "notes.md", func() bool { return false }, nil)
	if err == nil {
		t.Fatal("Consolidate succeeded despite an unwritable tenant directory; want an error")
	}

	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // test-only: restoring so t.TempDir() cleanup can remove it
		t.Fatalf("restore chmod: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "notes.md")) //nolint:gosec // dir is t.TempDir(); the path is test-constructed, not external input
	if rerr != nil {
		t.Fatalf("read source after failed consolidate: %v", rerr)
	}
	if string(got) != original {
		t.Errorf("source content = %q after a failed consolidate, want untouched original %q", got, original)
	}
}
