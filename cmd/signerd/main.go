// Command signerd holds the sign-only audit-chain signing key over a unix
// socket: nexusd may ask it to sign a digest, but can never read the key
// itself (README.md §5, Phase 5, task 5.1). The private key lives only in
// internal/audit/signerkey, a package this binary alone imports —
// tests/contract/boundaries_test.go's "nexusd must not import
// internal/audit/signerkey" rule is what makes that a structural property
// of the build rather than a convention nexusd could accidentally violate.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/audit/signerkey"
	"github.com/truongpx396/nexus-agent-demo/internal/dotenv"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/obs/profiling"
	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

const (
	defaultKeyPath    = ".dev/signer/ed25519.key"
	defaultSocketPath = ".dev/signerd.sock"

	// connDeadline bounds one request/response exchange. A client that connects
	// and never sends (or never reads) would otherwise hold a goroutine and a
	// file descriptor for the life of the process.
	connDeadline = 10 * time.Second
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envIntOr mirrors cmd/nexusd/main.go's own helper of the same name — a
// profiling knob degrades to "off" on a typo rather than failing the
// process closed (observability concern, not a security one).
func envIntOr(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

func main() {
	// See cmd/nexusd/main.go's identical call for why this comes first.
	if err := dotenv.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "signerd: %v\n", err)
	}
	obs.InitLogger("signerd")

	fmt.Printf("signerd %s (%s)\n", version.Version, version.GitCommit)

	key, err := signerkey.LoadOrGenerate(envOr("NEXUS_SIGNER_KEY_PATH", defaultKeyPath))
	if err != nil {
		fatalf("load signing key: %v", err)
	}
	log.Info().Any("key_id", key.KeyID).Msg("signerd: loaded signing key")

	// Profiling (docs/observability.md) — same opt-in knobs cmd/nexusd/infra.go
	// wires, under signerd's own address/app-name so the two processes never
	// collide on a bind port or a Pyroscope application selector. signerd's
	// own workload (sign a digest over a unix socket) is tiny, but a shared
	// obs helper costs nothing to apply consistently across both binaries.
	mutexFraction := envIntOr("NEXUS_PPROF_MUTEX_FRACTION", 0)
	blockRate := envIntOr("NEXUS_PPROF_BLOCK_RATE", 0)
	profiling.EnableMutexBlockProfiling(mutexFraction, blockRate)

	shutdownPprof, err := profiling.StartPprofServer(envOr("NEXUS_SIGNERD_PPROF_ADDR", ""))
	if err != nil {
		fatalf("start pprof server: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownPprof(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("signerd: shutdown pprof server")
		}
	}()

	profiler, err := profiling.StartPyroscope(profiling.PyroscopeConfig{
		ServerAddress:       envOr("NEXUS_PYROSCOPE_ADDR", ""),
		ApplicationName:     "signerd",
		Tags:                map[string]string{"git_commit": version.GitCommit},
		MutexBlockProfiling: mutexFraction > 0 || blockRate > 0,
	})
	if err != nil {
		fatalf("start pyroscope: %v", err)
	}
	if profiler != nil {
		defer func() {
			if err := profiler.Stop(); err != nil {
				log.Error().Err(err).Msg("signerd: stop pyroscope profiler")
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	socketPath := envOr("NEXUS_SIGNERD_SOCKET", defaultSocketPath)
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fatalf("remove stale socket %s: %v", socketPath, err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", socketPath)
	if err != nil {
		fatalf("listen on %s: %v", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		fatalf("chmod %s: %v", socketPath, err)
	}
	defer ln.Close() //nolint:errcheck // best-effort on the way out

	go func() {
		<-ctx.Done()
		_ = ln.Close() // unblocks Accept below
	}()

	fmt.Printf("listening on %s\n", socketPath)
	var handlers sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				handlers.Wait() // a signature already being written is finished, not cut off
				return          // orderly shutdown
			}
			log.Error().Err(err).Msg("signerd: accept")
			continue
		}
		handlers.Go(func() { handleConn(conn, key) })
	}
}

// handleConn serves exactly one request per connection, matching
// internal/audit.SignerClient's dial-per-call design — no framing beyond
// "one JSON line in, one JSON line out" is needed for a local socket.
func handleConn(conn net.Conn, key signerkey.Key) {
	defer conn.Close() //nolint:errcheck // nothing left to flush after writeResponse below
	_ = conn.SetDeadline(time.Now().Add(connDeadline))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	if !scanner.Scan() {
		return // client disconnected without sending a request
	}

	var req audit.Request
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		writeResponse(conn, audit.Response{Error: "invalid request: " + err.Error()})
		return
	}

	switch req.Op {
	case audit.OpSign:
		if len(req.Digest) == 0 {
			writeResponse(conn, audit.Response{Error: "sign: digest is required"})
			return
		}
		sig := key.Sign(req.Digest)
		writeResponse(conn, audit.Response{Signature: sig, KeyID: key.KeyID})
	case audit.OpPublicKey:
		writeResponse(conn, audit.Response{PublicKey: key.Public, KeyID: key.KeyID})
	default:
		writeResponse(conn, audit.Response{Error: fmt.Sprintf("unknown op %q", req.Op)})
	}
}

func writeResponse(conn net.Conn, resp audit.Response) {
	line, err := json.Marshal(resp)
	if err != nil {
		log.Error().Err(err).Msg("signerd: marshal response")
		return
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		log.Error().Err(err).Msg("signerd: write response")
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
