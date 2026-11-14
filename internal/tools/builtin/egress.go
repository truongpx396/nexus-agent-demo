package builtin

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Egress policy for the tools that fetch a URL on the model's behalf. Checking a
// hostname before the request is not enough on its own, in two ways:
//
//   - the check resolves the name and the request resolves it again, so a DNS name
//     that answers a public address to the check and a private one to the connection
//     reaches an internal service; and
//   - http.Client follows redirects by default, so an allow-listed host can send the
//     request to an address, or a host, the allowlist never named.
//
// newEgressClient closes both: the dialer vets the address it is about to connect to,
// after resolution, and every redirect is held to the same allowlist.

const (
	// egressMaxRedirects is how many redirects a fetch follows.
	egressMaxRedirects = 5
	// egressDialTimeout bounds the TCP connect, not the whole fetch.
	egressDialTimeout = 10 * time.Second
)

// privateBlocks are the ranges a fetch is refused unless private networks are
// allowed, beyond what net.IP's own predicates cover (loopback, RFC 1918 and ULA,
// link-local, multicast, unspecified): the current-network block, the carrier-grade
// NAT range, the benchmarking range, the reserved and broadcast range, and the NAT64
// prefix, which can carry any IPv4 address, private ones included.
//
// internal/hooks/httphook repeats this guard: that package has no reason to depend
// on this one, and it is a handful of CIDR literals, not shared logic worth a
// common package.
var privateBlocks = mustParseCIDRs(
	"0.0.0.0/8", "100.64.0.0/10", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	blocks := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, block, err := net.ParseCIDR(c)
		if err != nil {
			panic("builtin: invalid CIDR literal " + c + ": " + err.Error())
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// isPrivateIP reports whether ip is an address a fetch must not reach: anything but
// a public unicast address.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, b := range privateBlocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}

// newEgressClient returns the HTTP client a fetching tool uses when it is not
// handed one. allowed is the host allowlist (hostAllowed's patterns); allowPrivate is
// the test-only escape hatch the tools expose as AllowPrivateNetworks; timeout bounds
// the whole request.
//
// The transport never uses a proxy, whose own resolution would sidestep the dialer's
// check, and keeps no idle connections, so a client built per call leaves nothing
// behind.
func newEgressClient(allowed []string, allowPrivate bool, timeout time.Duration) *http.Client {
	tr := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone()
	}
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	dialer := &net.Dialer{
		Timeout: egressDialTimeout,
		// Control runs after the name is resolved, on the address about to be
		// connected to, so it cannot be told one thing and connected to another.
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("egress: unparseable address %q: %w", address, err)
			}
			ip := net.ParseIP(strings.TrimSuffix(strings.SplitN(host, "%", 2)[0], "."))
			if ip == nil || isPrivateIP(ip) {
				return fmt.Errorf("egress: refusing to connect to private or non-public address %s (SSRF guard)", address)
			}
			return nil
		},
	}
	tr.DialContext = dialer.DialContext

	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > egressMaxRedirects {
				return fmt.Errorf("egress: stopped after %d redirects", egressMaxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("egress: redirect to scheme %q not allowed", req.URL.Scheme)
			}
			if !hostAllowed(allowed, req.URL.Hostname()) {
				return fmt.Errorf("egress_denied: redirect to host %q, which is not on the allowlist", req.URL.Hostname())
			}
			return nil
		},
	}
}
