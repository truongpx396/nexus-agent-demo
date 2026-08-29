package audit

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
)

// Signer is what internal/audit.Chain needs to turn a chain/anchor hash
// into a signature: sign-only, never a way to read the key itself (task
// 5.1). SignerClient below is the real, unix-socket-backed implementation;
// tests use a local fake instead of standing up cmd/signerd.
type Signer interface {
	Sign(ctx context.Context, digest []byte) (signature []byte, keyID string, err error)
	PublicKey(ctx context.Context) (pub ed25519.PublicKey, keyID string, err error)
}

// SignerClient dials cmd/signerd's unix socket once per call. A local
// socket makes a persistent, reconnecting connection pool unneeded
// complexity for this demo — every call this package makes is already
// inside a Postgres transaction with its own latency budget, and a fresh
// dial to a local socket is sub-millisecond.
type SignerClient struct {
	SocketPath string
}

func NewSignerClient(socketPath string) *SignerClient {
	return &SignerClient{SocketPath: socketPath}
}

func (c *SignerClient) call(ctx context.Context, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return Response{}, fmt.Errorf("audit: dial signerd at %s: %w", c.SocketPath, err)
	}
	defer conn.Close() //nolint:errcheck // read-only after the exchange below; nothing to flush

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	line, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("audit: marshal signerd request: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return Response{}, fmt.Errorf("audit: write to signerd: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Response{}, fmt.Errorf("audit: read from signerd: %w", err)
		}
		return Response{}, fmt.Errorf("audit: signerd closed the connection with no response")
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("audit: parse signerd response: %w", err)
	}
	if resp.Error != "" {
		return Response{}, fmt.Errorf("audit: signerd: %s", resp.Error)
	}
	return resp, nil
}

func (c *SignerClient) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	resp, err := c.call(ctx, Request{Op: OpSign, Digest: digest})
	if err != nil {
		return nil, "", err
	}
	return resp.Signature, resp.KeyID, nil
}

func (c *SignerClient) PublicKey(ctx context.Context) (ed25519.PublicKey, string, error) {
	resp, err := c.call(ctx, Request{Op: OpPublicKey})
	if err != nil {
		return nil, "", err
	}
	return ed25519.PublicKey(resp.PublicKey), resp.KeyID, nil
}
