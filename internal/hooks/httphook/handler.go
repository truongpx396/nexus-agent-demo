// Package httphook is the http-hook adapter: it POSTs the hook request to an
// operator-configured endpoint behind an SSRF guard. It owns the net/http
// dependency so the hook port and dispatcher in internal/hooks — and the tool
// pipeline that imports them — do not.
package httphook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
)

// Handler POSTs the same hooks.Request shape CommandHandler sends over
// stdin, and expects the same hooks.Response shape back as the body.
type Handler struct {
	Client               *http.Client
	Resolver             *net.Resolver // overridable for tests
	AllowPrivateNetworks bool          // test-only escape hatch; production leaves this false
}

func (h Handler) Run(ctx context.Context, cfg hooks.Config, hctx hooks.Context) (hooks.Outcome, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return hooks.Outcome{}, fmt.Errorf("http hook %q: invalid url: %w", cfg.Name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return hooks.Outcome{}, fmt.Errorf("http hook %q: scheme %q not allowed", cfg.Name, u.Scheme)
	}

	if !h.AllowPrivateNetworks {
		resolver := h.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		ips, err := resolver.LookupIP(ctx, "ip", u.Hostname())
		if err != nil {
			return hooks.Outcome{}, fmt.Errorf("http hook %q: resolve %s: %w", cfg.Name, u.Hostname(), err)
		}
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return hooks.Outcome{}, fmt.Errorf("http hook %q: refuses to call private/loopback address %s (SSRF guard)", cfg.Name, ip)
			}
		}
	}

	payload, err := json.Marshal(hooks.Request{Event: string(cfg.Event), ToolID: hctx.ToolID, Input: hctx.Input})
	if err != nil {
		return hooks.Outcome{}, fmt.Errorf("marshal http hook request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload)) // cfg.URL is operator-authored hook config; the SSRF guard above bounds what it may resolve to
	if err != nil {
		return hooks.Outcome{}, fmt.Errorf("build http hook request: %w", err)
	}
	req.Header.Set("content-type", "application/json")

	client := h.Client
	if client == nil {
		client = newEgressClient(h.AllowPrivateNetworks, 10*time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return hooks.Outcome{}, fmt.Errorf("http hook %q: %w", cfg.Name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body; nothing to flush
	if resp.StatusCode/100 != 2 {
		return hooks.Outcome{}, fmt.Errorf("http hook %q: status %d", cfg.Name, resp.StatusCode)
	}

	var out hooks.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return hooks.Outcome{}, fmt.Errorf("http hook %q: parse response: %w", cfg.Name, err)
	}
	return hooks.Outcome{Decision: hooks.Decision(out.Decision), Reason: out.Reason, UpdatedInput: out.UpdatedInput}, nil
}
