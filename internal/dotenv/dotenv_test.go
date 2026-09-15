package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

// chdirTemp switches the working directory to a fresh temp dir for the
// duration of the test — Load always reads ".env" relative to cwd, so this
// is the seam every case below uses to control what it sees.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	return dir
}

func writeEnvFile(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	chdirTemp(t)
	if err := Load(); err != nil {
		t.Fatalf("Load with no .env present: %v", err)
	}
}

func TestLoad_SetsUnsetVariables(t *testing.T) {
	dir := chdirTemp(t)
	writeEnvFile(t, dir, "NEXUS_TEST_DOTENV_FOO=bar\n")
	t.Cleanup(func() { _ = os.Unsetenv("NEXUS_TEST_DOTENV_FOO") })

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("NEXUS_TEST_DOTENV_FOO"); got != "bar" {
		t.Errorf("NEXUS_TEST_DOTENV_FOO = %q, want %q", got, "bar")
	}
}

func TestLoad_RealEnvironmentWinsOverDotEnv(t *testing.T) {
	dir := chdirTemp(t)
	writeEnvFile(t, dir, "NEXUS_TEST_DOTENV_FOO=from-dotenv\n")
	t.Setenv("NEXUS_TEST_DOTENV_FOO", "from-real-env")

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("NEXUS_TEST_DOTENV_FOO"); got != "from-real-env" {
		t.Errorf("NEXUS_TEST_DOTENV_FOO = %q, want the real environment's value to win, got %q", got, "from-real-env")
	}
}

func TestLoad_SkipsBlankLinesAndComments(t *testing.T) {
	dir := chdirTemp(t)
	writeEnvFile(t, dir, "\n# a comment\n   \nNEXUS_TEST_DOTENV_FOO=bar\n# NEXUS_TEST_DOTENV_IGNORED=nope\n")
	t.Cleanup(func() { _ = os.Unsetenv("NEXUS_TEST_DOTENV_FOO"); _ = os.Unsetenv("NEXUS_TEST_DOTENV_IGNORED") })

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("NEXUS_TEST_DOTENV_FOO"); got != "bar" {
		t.Errorf("NEXUS_TEST_DOTENV_FOO = %q, want %q", got, "bar")
	}
	if got, present := os.LookupEnv("NEXUS_TEST_DOTENV_IGNORED"); present {
		t.Errorf("a commented-out line must not be set, got %q", got)
	}
}

func TestLoad_StripsMatchingQuotes(t *testing.T) {
	dir := chdirTemp(t)
	writeEnvFile(t, dir, "NEXUS_TEST_DOTENV_DQ=\"hello world\"\nNEXUS_TEST_DOTENV_SQ='hello single'\n")
	t.Cleanup(func() {
		_ = os.Unsetenv("NEXUS_TEST_DOTENV_DQ")
		_ = os.Unsetenv("NEXUS_TEST_DOTENV_SQ")
	})

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("NEXUS_TEST_DOTENV_DQ"); got != "hello world" {
		t.Errorf("double-quoted value = %q, want %q", got, "hello world")
	}
	if got := os.Getenv("NEXUS_TEST_DOTENV_SQ"); got != "hello single" {
		t.Errorf("single-quoted value = %q, want %q", got, "hello single")
	}
}
