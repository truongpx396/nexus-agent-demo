// docs/build-phases.md Phase 16, task 16.3: a second Isolation backend for
// platform/shell, redeeming the seam Isolation's own doc comment (sandbox.go)
// already named — "gvisor/kata as unshipped values... a stronger isolation
// backend is a config change later, not a schema or interface change" — with
// a real, actively maintained runtime (https://github.com/opensandbox-group/
// OpenSandbox) instead of an unshipped value. OpenSandboxSession satisfies
// internal/tools.SandboxExec exactly the same structural way SessionSandbox
// does (session.go's own doc comment): neither this file nor internal/tools
// needs to know about the other's declaration.
package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// OpenSandboxConfig is everything one OpenSandboxSession.Exec call needs —
// the OpenSandbox analogue of Config above.
type OpenSandboxConfig struct {
	// Domain is the OpenSandbox server's host:port (e.g. "localhost:8090").
	Domain string
	// Protocol is "http" or "https".
	Protocol string
	// APIKey authenticates against the server's OPEN-SANDBOX-API-KEY header
	// (empty is valid only against a server started with no api_key set —
	// this demo's dev-only posture, never a production one).
	APIKey string
	// Image defaults to "python:3.12-slim" — small, and enough of a POSIX
	// shell environment for what builtin.Shell's commands need, mirroring
	// Docker's own "alpine:3.20" default in Config.withDefaults above.
	Image  string
	Limits Limits
}

func (c OpenSandboxConfig) withDefaults() OpenSandboxConfig {
	if c.Image == "" {
		c.Image = "python:3.12-slim"
	}
	if c.Limits == (Limits{}) {
		c.Limits = DefaultLimits()
	}
	return c
}

// OpenSandboxSession binds a connection config to one session's Config, the
// OpenSandbox analogue of SessionSandbox.
type OpenSandboxSession struct {
	Config OpenSandboxConfig
}

// resourceLimits converts Limits' Docker-shaped units (NanoCPUs, whole
// bytes) into the Kubernetes-style quantity strings opensandbox.
// ResourceLimits expects ("500m", "512Mi") — the same unit family
// SandboxCreateOptions.ResourceLimits documents, and the shape the
// project's own deploy/docker-compose.agentic.yml config uses for this
// server. There is no PID-count knob here: OpenSandbox's ResourceLimits
// follows Kubernetes' cpu/memory resource-quantity shape, which has no PIDs
// resource type — unlike Docker.Exec's PidsLimit above, this backend simply
// has no equivalent to set. Its wall-clock timeout and cpu/memory ceilings
// are what it actually enforces.
func resourceLimits(l Limits) opensandbox.ResourceLimits {
	millicores := l.NanoCPUs / 1_000_000 // 1 NanoCPU unit = 1e-9 CPU; 1 millicore = 1e-3 CPU
	if millicores <= 0 {
		millicores = 1000
	}
	mebibytes := l.MemoryBytes / (1 << 20)
	if mebibytes <= 0 {
		mebibytes = 512
	}
	return opensandbox.ResourceLimits{
		"cpu":    fmt.Sprintf("%dm", millicores),
		"memory": fmt.Sprintf("%dMi", mebibytes),
	}
}

// Exec runs cmd (via `/bin/sh -c`) to completion inside a FRESH OpenSandbox
// sandbox under cfg: a deny-by-default NetworkPolicy (the same --network
// none posture Docker's createContainer already enforces, task 5.12/5.13),
// UseServerProxy so a host-run nexusd never needs the server's dynamic
// sandbox-port range published (deploy/docker-compose.agentic.yml's own
// comment on this), and EndpointHostRewrite for the
// host.docker.internal-inside-the-server-container topology
// docker-compose.example.yaml's own [docker] host_ip setting produces.
// Every path — clean exit, a breach, or a mid-setup error — kills the
// sandbox before returning, the same "breach -> terminate + reclaim"
// contract Docker.Exec's own doc comment names.
func (s OpenSandboxSession) Exec(ctx context.Context, cmd string) (output string, exitCode int, breach string, err error) {
	cfg := s.Config.withDefaults()

	connCfg := opensandbox.ConnectionConfig{
		Domain:              cfg.Domain,
		Protocol:            cfg.Protocol,
		APIKey:              cfg.APIKey,
		UseServerProxy:      true,
		EndpointHostRewrite: map[string]string{"host.docker.internal": "localhost"},
	}

	wallSeconds := int(cfg.Limits.WallTimeout.Seconds())
	if wallSeconds <= 0 {
		wallSeconds = int(DefaultLimits().WallTimeout.Seconds())
	}

	sbx, createErr := opensandbox.CreateSandbox(ctx, connCfg, opensandbox.SandboxCreateOptions{
		Image:          cfg.Image,
		Entrypoint:     []string{"/bin/sh"},
		ResourceLimits: resourceLimits(cfg.Limits),
		TimeoutSeconds: &wallSeconds,
		NetworkPolicy:  &opensandbox.NetworkPolicy{DefaultAction: "deny"},
	})
	if createErr != nil {
		return "", 0, "", fmt.Errorf("opensandbox: create sandbox: %w", createErr)
	}
	defer func() {
		killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sbx.Kill(killCtx) // best-effort, matching Docker.reclaim's own posture
	}()

	waitCtx, cancel := context.WithTimeout(ctx, cfg.Limits.WallTimeout)
	defer cancel()

	exec, runErr := sbx.RunCommandWithOpts(waitCtx, opensandbox.RunCommandRequest{
		Command: cmd,
		Timeout: cfg.Limits.WallTimeout.Milliseconds(),
	}, nil)

	combined := combineOutput(exec)
	if runErr != nil {
		if waitCtx.Err() != nil {
			// Client-side backstop, the same role Docker.Exec's own waitCtx
			// plays: whatever the server-side Timeout above did or didn't
			// enforce, this call never blocks past cfg.Limits.WallTimeout.
			return combined, 0, string(BreachTimeout), nil
		}
		return combined, 0, "", fmt.Errorf("opensandbox: run command: %w", runErr)
	}

	code := 0
	if exec != nil && exec.ExitCode != nil {
		code = *exec.ExitCode
	}
	return combined, code, "", nil
}

// combineOutput flattens an *opensandbox.Execution's separate stdout/stderr
// streams into the single "combined stdout+stderr" string
// tools.SandboxExec's Exec signature documents — the same shape
// SessionSandbox.Exec above gets for free from Docker's Tty:true collapsing
// the two streams server-side.
func combineOutput(exec *opensandbox.Execution) string {
	if exec == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range exec.Stdout {
		b.WriteString(m.Text)
	}
	for _, m := range exec.Stderr {
		b.WriteString(m.Text)
	}
	if exec.Error != nil {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(exec.Error.Name)
		b.WriteString(": ")
		b.WriteString(exec.Error.Value)
	}
	return b.String()
}
