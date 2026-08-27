package version

import "testing"

func TestVersionDefaults(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must never be empty")
	}
	if GitCommit == "" {
		t.Fatal("GitCommit must never be empty")
	}
}
