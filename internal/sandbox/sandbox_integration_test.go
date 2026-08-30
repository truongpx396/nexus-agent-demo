//go:build integration

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestDocker(t *testing.T) *Docker {
	t.Helper()
	d, err := NewDocker()
	if err != nil {
		t.Skipf("no docker daemon reachable: %v", err)
	}
	return d
}

func TestDocker_ExecRunsCommandAndCapturesOutput(t *testing.T) {
	d := newTestDocker(t)
	cfg := Config{WorkspaceDir: t.TempDir()}

	res, err := d.Exec(context.Background(), cfg, "echo hello-from-sandbox")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Breach != BreachNone {
		t.Fatalf("Breach = %q, want none", res.Breach)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello-from-sandbox") {
		t.Fatalf("Output = %q, want it to contain the echoed text", res.Output)
	}
}

func TestDocker_ExecCapturesNonZeroExit(t *testing.T) {
	d := newTestDocker(t)
	cfg := Config{WorkspaceDir: t.TempDir()}

	res, err := d.Exec(context.Background(), cfg, "exit 7")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
}

// TestDocker_ExecNetworkIsDefaultDeny is task 5.13's own promise: sandboxed
// tools get the deny set for free via --network none.
func TestDocker_ExecNetworkIsDefaultDeny(t *testing.T) {
	d := newTestDocker(t)
	cfg := Config{WorkspaceDir: t.TempDir()}

	// wget/curl may not exist in alpine by default; a raw TCP attempt via
	// /dev/tcp (ash supports this) fails immediately with no network stack
	// beyond loopback, regardless of what's installed.
	res, err := d.Exec(context.Background(), cfg, "echo > /dev/tcp/1.1.1.1/80 2>&1; echo exit=$?")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Output, "exit=0") {
		t.Fatalf("network reached out successfully despite --network none: %q", res.Output)
	}
}

func TestDocker_ExecTimeoutIsBreach(t *testing.T) {
	d := newTestDocker(t)
	cfg := Config{WorkspaceDir: t.TempDir(), Limits: Limits{NanoCPUs: 1_000_000_000, MemoryBytes: 128 << 20, PIDs: 64, WallTimeout: 2 * time.Second}}

	res, err := d.Exec(context.Background(), cfg, "sleep 30")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Breach != BreachTimeout {
		t.Fatalf("Breach = %q, want %q", res.Breach, BreachTimeout)
	}
}

func TestDocker_ExecWorkspaceBindMountRoundTrips(t *testing.T) {
	d := newTestDocker(t)
	workspace := t.TempDir()
	cfg := Config{WorkspaceDir: workspace}

	if _, err := d.Exec(context.Background(), cfg, "echo written-from-inside > /workspace/out.txt"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	raw, err := os.ReadFile(workspace + "/out.txt") //nolint:gosec // workspace is this test's own t.TempDir(), never external input
	if err != nil {
		t.Fatalf("read host-side file: %v", err)
	}
	if content := strings.TrimSpace(string(raw)); content != "written-from-inside" {
		t.Fatalf("content = %q, want %q", content, "written-from-inside")
	}
}
