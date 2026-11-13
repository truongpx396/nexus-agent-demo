package httphook

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// An http hook's target is operator config, but a hostname checked before the
// request is not the address the request connects to (it is resolved again), and
// http.Client follows redirects by default, so a hook endpoint could send the call
// anywhere. newEgressClient vets the address actually dialled, after resolution, and
// lets a redirect stay only on the host the hook was configured with.
//
// internal/tools/builtin has the same guard for the fetching tools; this package has
// no reason to depend on that one, and it is a few CIDR literals and a dialer, not
// shared logic worth a common package.

const (
	maxRedirects = 5
	dialTimeout  = 10 * time.Second
)

// privateBlocks are the ranges a hook is refused, beyond what net.IP's own
// predicates cover (loopback, RFC 1918 and ULA, link-local, multicast, unspecified):
// the current-network block, carrier-grade NAT, the benchmarking range, the reserved
// and broadcast range, and the NAT64 prefix, which can carry any IPv4 address.
var privateBlocks = mustParseCIDRs(
	"0.0.0.0/8", "100.64.0.0/10", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96",
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

// isPrivateIP reports whether ip is anything but a public unicast address.
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

// newEgressClient is the client a Handler uses when it is not handed one. The
// transport never uses a proxy, whose own resolution would sidestep the dialer's
// check, and keeps no idle connections, so a client built per call leaves nothing
// behind.
func newEgressClient(allowPrivate bool, timeout time.Duration) *http.Client {
	tr := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone()
	}
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	dialer := &net.Dialer{
		Timeout: dialTimeout,
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
			ip := net.ParseIP(strings.SplitN(host, "%", 2)[0])
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
			if len(via) > maxRedirects {
				return fmt.Errorf("egress: stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("egress: redirect to scheme %q not allowed", req.URL.Scheme)
			}
			if first := via[0].URL.Hostname(); req.URL.Hostname() != first {
				return fmt.Errorf("egress: redirect from host %q to %q; a hook only talks to the host it is configured with", first, req.URL.Hostname())
			}
			return nil
		},
	}
}
