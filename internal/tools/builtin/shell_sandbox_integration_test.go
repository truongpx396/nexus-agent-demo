//go:build integration

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/sandbox"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// TestShell_CallSandboxedRoutesThroughDocker is the end-to-end path README
// task 5.12 actually replaces: rc.Sandbox set (as internal/tools/
// pipeline.go's SandboxFactory wiring would set it, README task 5.12) must
// make Shell.Call run the command inside Docker rather than the local
// process — proven here by a marker file that only a REAL, isolated
// container's own /workspace bind mount could produce.
func TestShell_CallSandboxedRoutesThroughDocker(t *testing.T) {
	docker, err := sandbox.NewDocker()
	if err != nil {
		t.Skipf("no docker daemon reachable: %v", err)
	}
	workspace := t.TempDir()
	sess := sandbox.SessionSandbox{Docker: docker, Config: sandbox.Config{WorkspaceDir: workspace}}

	s := Shell{}
	in, err := json.Marshal(map[string]string{"cmd": "echo from-sandbox > marker.txt; hostname > hostname.txt"})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	result, err := s.Call(context.Background(), in, tools.RunContext{Sandbox: sess, WorkspaceDir: workspace})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.IsError {
		t.Fatalf("Call() = %+v, want success", result)
	}

	marker, err := os.ReadFile(workspace + "/marker.txt") //nolint:gosec // workspace is this test's own t.TempDir(), never external input
	if err != nil {
		t.Fatalf("marker.txt not found on the host workspace — the container's /workspace bind mount didn't round-trip: %v", err)
	}
	if strings.TrimSpace(string(marker)) != "from-sandbox" {
		t.Fatalf("marker.txt = %q, want %q", marker, "from-sandbox")
	}

	// The container's own hostname is its (short, random) container ID,
	// never this test process's actual host name — proof the command ran
	// isolated, not via a local os/exec fallback.
	hostHostname, _ := os.Hostname()
	containerHostname, err := os.ReadFile(workspace + "/hostname.txt") //nolint:gosec // workspace is this test's own t.TempDir(), never external input
	if err != nil {
		t.Fatalf("hostname.txt not found: %v", err)
	}
	if strings.TrimSpace(string(containerHostname)) == hostHostname {
		t.Fatal("container hostname matches the test process's own host — command did not actually run inside a container")
	}
}
