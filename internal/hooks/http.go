package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// privateBlocks is the set of ranges an http hook is refused to call unless
// AllowPrivateNetworks is set — loopback, RFC1918/ULA, link-local, and the
// "current network" block. This is the SSRF guard README task 3.11 names:
// an http hook target is tenant/operator config, but Config.URL can still be
// templated from tool input in a future phase, so the guard belongs here,
// not at the config-authoring boundary.
var privateBlocks = mustParseCIDRs(
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"169.254.0.0/16", "0.0.0.0/8",
	"::1/128", "fc00::/7", "fe80::/10",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	blocks := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, block, err := net.ParseCIDR(c)
		if err != nil {
			panic("hooks: invalid CIDR literal " + c + ": " + err.Error())
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func isPrivateIP(ip net.IP) bool {
	for _, b := range privateBlocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}

// HTTPHandler POSTs the same commandRequest shape CommandHandler sends over
// stdin, and expects the same commandResponse shape back as the body.
type HTTPHandler struct {
	Client               *http.Client
	Resolver             *net.Resolver // overridable for tests
	AllowPrivateNetworks bool          // test-only escape hatch; production leaves this false
}

func (h HTTPHandler) Run(ctx context.Context, cfg Config, hctx Context) (Outcome, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return Outcome{}, fmt.Errorf("http hook %q: invalid url: %w", cfg.Name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Outcome{}, fmt.Errorf("http hook %q: scheme %q not allowed", cfg.Name, u.Scheme)
	}

	if !h.AllowPrivateNetworks {
		resolver := h.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		ips, err := resolver.LookupIP(ctx, "ip", u.Hostname())
		if err != nil {
			return Outcome{}, fmt.Errorf("http hook %q: resolve %s: %w", cfg.Name, u.Hostname(), err)
		}
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return Outcome{}, fmt.Errorf("http hook %q: refuses to call private/loopback address %s (SSRF guard)", cfg.Name, ip)
			}
		}
	}

	payload, err := json.Marshal(commandRequest{Event: string(cfg.Event), ToolID: hctx.ToolID, Input: hctx.Input})
	if err != nil {
		return Outcome{}, fmt.Errorf("marshal http hook request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload)) //nolint:gosec // cfg.URL is operator-authored hook config; the SSRF guard above bounds what it may resolve to
	if err != nil {
		return Outcome{}, fmt.Errorf("build http hook request: %w", err)
	}
	req.Header.Set("content-type", "application/json")

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Outcome{}, fmt.Errorf("http hook %q: %w", cfg.Name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body; nothing to flush
	if resp.StatusCode/100 != 2 {
		return Outcome{}, fmt.Errorf("http hook %q: status %d", cfg.Name, resp.StatusCode)
	}

	var out commandResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Outcome{}, fmt.Errorf("http hook %q: parse response: %w", cfg.Name, err)
	}
	return Outcome{Decision: Decision(out.Decision), Reason: out.Reason, UpdatedInput: out.UpdatedInput}, nil
}
