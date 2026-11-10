// Command nexusd is the single-binary data+control plane: the kernel loop,
// the harness, and the REST surface, all in one process (see README.md §4).
// Phase 1 gave it two admin subcommands (migrate, seed); Phase 2 adds the
// kernel loop and REST surface, wired together here — this package is the
// one place in the binary allowed to import both kernel/ and
// internal/surfaces/rest (tests/contract/boundaries_test.go forbids the
// surface from importing the kernel directly; the composition root is
// exempt because it isn't either of those packages). It is split across
// several files by responsibility (serve.go's process wiring, the cli_*.go
// admin subcommands, ports.go's rest/builtin adapters, etc.) — every file
// here shares that same exemption, since Go's import graph is scoped to the
// package, not the file.
package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/dotenv"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
)

const (
	// defaultMigrateDSN connects DIRECTLY to Postgres (bypassing PgBouncer):
	// migrations are an admin operation, not a tenant-scoped one, and DDL
	// has no reason to go through the transaction-pooling tier the runtime
	// path depends on.
	defaultMigrateDSN = "postgres://nexus:nexus@localhost:5433/nexus" //nolint:gosec // local-dev default matching deploy/docker-compose.yml, not a real credential
	// defaultAppDSN connects THROUGH PgBouncer in transaction-pooling
	// mode, as nexus_app — an ordinary role with no RLS bypass (migrations/
	// 0000_app_role.sql). nexus, the migration role, is a superuser and
	// would silently see every tenant's rows regardless of RLS.
	defaultAppDSN = "postgres://nexus_app:nexus_app@localhost:6432/nexus" //nolint:gosec // local-dev default matching deploy/docker-compose.yml, not a real credential
	// defaultRedisAddr matches deploy/docker-compose.yml's host mapping
	// (6380, not Redis's usual 6379 — see that file's own comment on why).
	// internal/cost.Gate uses it for the tenant-ceiling epoch-marked
	// counter (README task 4.4); nothing else in this binary touches Redis.
	defaultRedisAddr = "localhost:6380"
	// defaultSignerdSocket matches cmd/signerd's own default — nexusd
	// dials it as a Signer client (internal/audit.SignerClient); it never
	// reads the private key itself (README task 5.1,
	// tests/contract/boundaries_test.go's dedicated rule).
	defaultSignerdSocket = ".dev/signerd.sock"
	// anchorInterval is how often the periodic anchor+verify pass (task
	// 5.3's "scheduled verifier") runs against every tenant.
	anchorInterval = 5 * time.Minute
	// teamBackstopSweepInterval is how often startTeamBackstopLoop checks
	// every tenant for an 'active' team past teamBackstopWindow (README task
	// 9.9's own wall-clock trigger); teamBackstopWindow is that window
	// itself — generous for a demo (a stuck team is a bug to notice, not a
	// tight SLA to enforce).
	teamBackstopSweepInterval = 5 * time.Minute
	teamBackstopWindow        = 30 * time.Minute
	// idleConversationSweepInterval is how often startIdleConversationSweepLoop
	// checks every tenant for a conversational session (kernel.RunConfig.
	// Conversational) sitting in eventlog.SessionStatusAwaitingInput past
	// idleConversationWindow with nobody sending the next message;
	// idleConversationWindow is that window itself — the backstop that
	// keeps an abandoned chat from holding its taint state, cost-gate
	// reservations, and open SSE subscribers forever (kernel.
	// ReasonIdleTimeout's own doc comment). Mirrors teamBackstopSweepInterval/
	// teamBackstopWindow's own precedent exactly.
	idleConversationSweepInterval = 5 * time.Minute
	idleConversationWindow        = 30 * time.Minute
)

// devMode is set once, at the top of main(), from a --dev flag scanned out
// of os.Args before the subcommand switch (README task 13.11/F12): with it,
// every zero-setup default this demo has always had keeps working
// (auto-generated KEK and AuthN signing key, localhost DSNs); without it,
// serve() fails closed on anything security/connectivity-critical left
// unset rather than silently minting a fresh key or dialing a
// developer-convenience default in what's presumed to be a real deployment.
// A package-level var (not threaded through every subcommand's own flag
// set) because loadOrGenerateKEK is called from both serve() and runErase,
// and the signing-key equivalent only from serve() and runToken — a single
// process-wide switch is simpler than plumbing a bool through each.
var devMode bool

// command is one nexusd subcommand: it receives the process context (cancelled by
// Ctrl-C or SIGTERM) and the arguments after its name.
type command func(ctx context.Context, args []string) error

// commands is every subcommand, and the one place the usage text comes from.
var commands = map[string]command{
	"serve":        runServe,
	"migrate":      func(ctx context.Context, _ []string) error { return runMigrate(ctx) },
	"seed":         runSeed,
	"verify-chain": runVerifyChain,
	"dashboard":    runDashboard,
	"go-live":      runGoLive,
	"erase":        runErase,
	"ingest":       runIngest,
	"token":        runToken,
}

func main() { os.Exit(run(os.Args[1:])) }

// parseArgs strips the global --dev flag, wherever it appears, and splits off the
// subcommand name; with no subcommand the process serves.
func parseArgs(args []string) (name string, rest []string, dev bool) {
	name = "serve"
	named := false
	for _, a := range args {
		switch {
		case a == "--dev":
			dev = true
		case !named:
			name, named = a, true
		default:
			rest = append(rest, a)
		}
	}
	return name, rest, dev
}

// run dispatches a subcommand and returns the process's exit code, so main's one
// os.Exit happens after run's defers, not instead of them. With no subcommand it
// serves.
func run(args []string) int {
	// Before InitLogger, before every other envOr/os.Getenv call this file
	// and the packages it wires up make: .env.example (repo root) documents
	// what dotenv.Load fills in here, and only when the real environment
	// doesn't already have it — a plain `FOO=bar make run` still wins.
	if err := dotenv.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "nexusd: %v\n", err)
	}
	obs.InitLogger("nexusd")

	// Ctrl-C or SIGTERM cancels a long subcommand (migrate, ingest, verify-chain,
	// serve) cleanly instead of killing it mid-statement.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name, rest, dev := parseArgs(args)
	devMode = dev
	cmd, ok := commands[name]
	if !ok {
		// A mistyped subcommand must not boot a server as a side effect.
		fmt.Fprintf(os.Stderr, "nexusd: unknown command %q (commands: %s)\n", name, strings.Join(slices.Sorted(maps.Keys(commands)), ", "))
		return 2
	}
	if err := cmd(ctx, rest); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// envIntOr parses key as a base-10 int, falling back to fallback on unset OR
// unparseable — like envOr's own sibling env vars, a profiling knob
// (NEXUS_PPROF_MUTEX_FRACTION/NEXUS_PPROF_BLOCK_RATE) is an observability
// concern, not a security one, so a typo degrades to "profiling stays off"
// rather than failing the process closed.
func envIntOr(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
