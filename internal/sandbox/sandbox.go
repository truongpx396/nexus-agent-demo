// Package sandbox implements the hardened exec environment README.md §5,
// task 5.12 names: Docker, `--network none`, hard CPU/mem/PID/wall limits,
// a per-session workspace bind mount — replacing the "honest, unsandboxed
// interim" internal/tools/builtin.Shell and PipelineConfig.WorkspaceRoot's
// own doc comments both name this package as the thing that would replace
// them. Isolation carries "gvisor"/"kata" as unshipped values (the task's
// own wording) so a stronger isolation backend is a config change later,
// not a schema or interface change.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Isolation names the exec backend a Sandbox row uses.
type Isolation string

const (
	IsolationDocker Isolation = "docker" // shipped — the only backend Exec below implements
	IsolationGVisor Isolation = "gvisor" // unshipped — a seam, not a promise
	IsolationKata   Isolation = "kata"   // unshipped — a seam, not a promise
)

// Limits are the hard resource ceilings every call is bound by — never
// advisory, never negotiated with the running command.
type Limits struct {
	NanoCPUs    int64 // CPU quota in units of 10^-9 CPUs (container.Resources.NanoCPUs' own unit)
	MemoryBytes int64
	PIDs        int64
	WallTimeout time.Duration
}

// DefaultLimits is a conservative default for one shell call: one core,
// 512MiB, 128 PIDs, 30s wall clock.
func DefaultLimits() Limits {
	return Limits{NanoCPUs: 1_000_000_000, MemoryBytes: 512 << 20, PIDs: 128, WallTimeout: 30 * time.Second}
}

// Config is everything one Exec call needs.
type Config struct {
	// Image defaults to "alpine:3.20" — small, and enough of a POSIX shell
	// environment for what builtin.Shell's commands need.
	Image string
	// WorkspaceDir is the HOST directory bind-mounted at /workspace inside
	// the container — the same per-session directory
	// tools.PipelineConfig.WorkspaceRoot already scopes builtin filesystem
	// tools to (internal/tools/pipeline.go), now the sandbox's own
	// boundary too rather than the process's own filesystem.
	WorkspaceDir string
	Limits       Limits
}

func (c Config) withDefaults() Config {
	if c.Image == "" {
		c.Image = "alpine:3.20"
	}
	if c.Limits == (Limits{}) {
		c.Limits = DefaultLimits()
	}
	return c
}

// BreachKind classifies why a call was cut short — the two conditions this
// package can actually observe post-hoc via the Docker API. A PIDs-limit
// breach is a preventive control (the kernel refuses the (N+1)th process
// inside the container) rather than something with its own post-hoc
// signal, so it surfaces as an ordinary non-zero ExitCode from whatever
// command hit it, not a third BreachKind — an honest simplification, not a
// gap: the limit itself is still enforced.
type BreachKind string

const (
	BreachNone    BreachKind = ""
	BreachTimeout BreachKind = "timeout"
	BreachOOM     BreachKind = "oom"
)

// Result is what Exec returns for one call.
type Result struct {
	Output   string // combined stdout+stderr
	ExitCode int
	Breach   BreachKind
}

// Docker is the shipped Isolation=IsolationDocker backend.
type Docker struct {
	Client *client.Client
}

// NewDocker connects using the standard Docker environment variables
// (DOCKER_HOST, etc.) — the same convention `docker` CLI itself uses, so
// this needs no nexusd-specific configuration beyond a running daemon.
func NewDocker() (*Docker, error) {
	cli, err := client.New(client.WithHostFromEnv()) // API-version negotiation is on by default
	if err != nil {
		return nil, fmt.Errorf("sandbox: connect to docker: %w", err)
	}
	return &Docker{Client: cli}, nil
}

// Exec runs cmd (via `/bin/sh -c`) to completion inside a FRESH container
// under cfg: `--network none`, hard CPU/mem/PID limits, cfg.WorkspaceDir
// bind-mounted at /workspace as the working directory. There is no warm
// pool and no exec-into-a-long-lived-container path (README §2's
// infrastructure-collapse table: "Docker sandbox (no warm pool)") — one
// container per call is the honest, simple interim this demo needs, not a
// performance-tuned runtime. Every path — clean exit, a breach, or a
// mid-setup error — reclaims (force-removes) the container before
// returning; nothing this call creates outlives it, matching task 5.12's
// "breach -> terminate + reclaim."
func (d *Docker) Exec(ctx context.Context, cfg Config, cmd string) (Result, error) {
	cfg = cfg.withDefaults()

	created, err := d.createContainer(ctx, cfg, cmd)
	if err != nil {
		return Result{}, err
	}
	defer d.reclaim(created.ID)

	if _, err := d.Client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return Result{}, fmt.Errorf("sandbox: start container %s: %w", created.ID, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, cfg.Limits.WallTimeout)
	defer cancel()
	wait := d.Client.ContainerWait(waitCtx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})

	var exitCode int
	var breach BreachKind
	select {
	case <-waitCtx.Done():
		_, _ = d.Client.ContainerKill(ctx, created.ID, client.ContainerKillOptions{})
		breach = BreachTimeout
	case werr := <-wait.Error:
		if werr != nil {
			return Result{}, fmt.Errorf("sandbox: wait for container %s: %w", created.ID, werr)
		}
	case wr := <-wait.Result:
		exitCode = int(wr.StatusCode)
	}

	// Inspect AFTER the wait/kill resolves — State.OOMKilled and the final
	// ExitCode are only meaningful once the container has actually stopped.
	if insp, ierr := d.Client.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{}); ierr == nil && insp.Container.State != nil {
		if insp.Container.State.OOMKilled {
			breach = BreachOOM
		}
		if breach != BreachTimeout {
			exitCode = insp.Container.State.ExitCode
		}
	}

	output := ""
	if logs, lerr := d.Client.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true}); lerr == nil {
		b, _ := io.ReadAll(logs)
		_ = logs.Close()
		output = string(b)
	}

	return Result{Output: output, ExitCode: exitCode, Breach: breach}, nil
}

// createContainer creates the container Exec runs, pulling cfg.Image once
// and retrying if the daemon doesn't already have it — a fresh machine with
// Docker installed but no images pulled must still work, the same
// zero-setup expectation NEXUS_PROVIDER=fake and loadOrGenerateKEK already
// hold cmd/nexusd to.
func (d *Docker) createContainer(ctx context.Context, cfg Config, cmd string) (client.ContainerCreateResult, error) {
	opts := client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      cfg.Image,
			Cmd:        []string{"/bin/sh", "-c", cmd},
			WorkingDir: "/workspace",
			// Tty collapses stdout/stderr into ONE stream in the logs API
			// response, avoiding the stream-multiplexing frame format a
			// non-tty container's logs otherwise use — matching
			// builtin.Shell's own CombinedOutput() behavior exactly.
			Tty: true,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "none", // README tasks 5.12/5.13: default-deny network from inside the sandbox
			Binds:       []string{cfg.WorkspaceDir + ":/workspace"},
			Resources: container.Resources{
				NanoCPUs:  cfg.Limits.NanoCPUs,
				Memory:    cfg.Limits.MemoryBytes,
				PidsLimit: &cfg.Limits.PIDs,
			},
		},
	}

	created, err := d.Client.ContainerCreate(ctx, opts)
	if err == nil {
		return created, nil
	}
	if !cerrdefs.IsNotFound(err) {
		return client.ContainerCreateResult{}, fmt.Errorf("sandbox: create container: %w", err)
	}

	pull, perr := d.Client.ImagePull(ctx, cfg.Image, client.ImagePullOptions{})
	if perr != nil {
		return client.ContainerCreateResult{}, fmt.Errorf("sandbox: pull image %s: %w", cfg.Image, perr)
	}
	if werr := pull.Wait(ctx); werr != nil {
		return client.ContainerCreateResult{}, fmt.Errorf("sandbox: pull image %s: %w", cfg.Image, werr)
	}

	created, err = d.Client.ContainerCreate(ctx, opts)
	if err != nil {
		return client.ContainerCreateResult{}, fmt.Errorf("sandbox: create container after pulling %s: %w", cfg.Image, err)
	}
	return created, nil
}

// reclaim force-removes containerID, best-effort, on a context independent
// of the caller's (which may already be canceled/timed out by the time
// Exec's own deferred cleanup runs).
func (d *Docker) reclaim(containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = d.Client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
}
