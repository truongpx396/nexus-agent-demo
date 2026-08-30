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
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/audit/signerkey"
	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

const defaultKeyPath = ".dev/signer/ed25519.key"
const defaultSocketPath = ".dev/signerd.sock"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	fmt.Printf("signerd %s (%s)\n", version.Version, version.GitCommit)

	key, err := signerkey.LoadOrGenerate(envOr("NEXUS_SIGNER_KEY_PATH", defaultKeyPath))
	if err != nil {
		fatalf("load signing key: %v", err)
	}
	slog.Info("signerd: loaded signing key", "key_id", key.KeyID)

	socketPath := envOr("NEXUS_SIGNERD_SOCKET", defaultSocketPath)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		fatalf("remove stale socket %s: %v", socketPath, err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		fatalf("listen on %s: %v", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		fatalf("chmod %s: %v", socketPath, err)
	}
	defer ln.Close() //nolint:errcheck // best-effort on the way out

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close() // unblocks Accept below
	}()

	fmt.Printf("listening on %s\n", socketPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // orderly shutdown
			}
			slog.Error("signerd: accept", "error", err)
			continue
		}
		go handleConn(conn, key)
	}
}

// handleConn serves exactly one request per connection, matching
// internal/audit.SignerClient's dial-per-call design — no framing beyond
// "one JSON line in, one JSON line out" is needed for a local socket.
func handleConn(conn net.Conn, key signerkey.Key) {
	defer conn.Close() //nolint:errcheck // nothing left to flush after writeResponse below

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
		slog.Error("signerd: marshal response", "error", err)
		return
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		slog.Error("signerd: write response", "error", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
