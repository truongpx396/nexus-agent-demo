package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestIsPrivateIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true,
		"169.254.169.254": true, "fe80::1": true, "fc00::1": true, "224.0.0.1": true, "ff02::1": true,
		"0.0.0.0": true, "0.1.2.3": true, "::": true,
		"100.64.0.1": true, "100.127.255.255": true, // carrier-grade NAT
		"198.18.0.1": true, "198.19.255.255": true, // benchmarking
		"240.0.0.1": true, "255.255.255.255": true, // reserved and broadcast
		"64:ff9b::7f00:1":  true,                          // NAT64 wrapping 127.0.0.1
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, // IPv4-mapped
		"8.8.8.8": false, "1.1.1.1": false, "93.184.216.34": false, "100.63.255.255": false, "100.128.0.1": false,
		"198.17.255.255": false, "198.20.0.1": false, "2606:4700:4700::1111": false, "2001:4860:4860::8888": false,
	} {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Fatalf("fixture %q does not parse", ip)
		}
		if got := isPrivateIP(parsed); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// localhostURL is srv's base URL under the name "localhost" instead of its address.
func localhostURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(hostOf(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return "http://localhost:" + port
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// The dialer vets the address it is about to connect to. A name that resolves to a
// private address is refused at connect time, whatever an earlier check was told.
func TestEgressClient_RefusesAPrivateAddressWhenDialling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("internal")) }))
	defer srv.Close()
	client := newEgressClient([]string{"*"}, false, 2*time.Second)

	_, err := client.Get(srv.URL) //nolint:noctx // a bounded test request
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("Get(%s) error = %v, want a refusal that mentions the address", srv.URL, err)
	}

	// The same server reached by name: resolution happens in the dialer, after
	// any pre-check, and the connection is still refused.
	_, port, _ := net.SplitHostPort(hostOf(t, srv.URL))
	_, err = client.Get("http://localhost:" + port + "/") //nolint:noctx // a bounded test request
	if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("Get(localhost) error = %v, want the dialer's SSRF refusal", err)
	}

	open := newEgressClient([]string{"*"}, true, 2*time.Second)
	resp, err := open.Get(srv.URL) //nolint:noctx // a bounded test request
	if err != nil {
		t.Fatalf("with private networks allowed, Get = %v", err)
	}
	_ = resp.Body.Close()
}

// A redirect from an allow-listed host to a host the allowlist does not name must
// not be followed.
func TestWebFetch_ARedirectToAHostOffTheAllowlistIsRefused(t *testing.T) {
	var hits int
	var viaLocalhost string // the same server, reached under a name the allowlist does not list
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/inside" {
			_, _ = w.Write([]byte("never reached"))
			return
		}
		http.Redirect(w, r, viaLocalhost+"/inside", http.StatusFound)
	}))
	defer srv.Close()
	viaLocalhost = localhostURL(t, srv)

	w := WebFetch{AllowedHosts: []string{"127.0.0.1"}, AllowPrivateNetworks: true}
	in, _ := json.Marshal(map[string]string{"url": srv.URL + "/start"})
	result, err := w.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Reason, "egress_denied") || !strings.Contains(result.Reason, "localhost") {
		t.Fatalf("Call() = %+v, want an egress_denied error naming the redirect target", result)
	}
	if hits != 1 {
		t.Fatalf("the server was hit %d times, want 1: the redirect target must not be requested", hits)
	}

	// An allowlist that does name the target follows it.
	w.AllowedHosts = []string{"127.0.0.1", "localhost"}
	result, err = w.Call(context.Background(), in, tools.RunContext{})
	if err != nil || result.IsError {
		t.Fatalf("with both hosts allowed, Call() = %+v, %v", result, err)
	}
}

// ConnectorFetch sends a bearer token, so it has the same redirect rule.
func TestConnectorFetch_ARedirectToAHostOffTheAllowlistIsRefused(t *testing.T) {
	var viaLocalhost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inside" {
			t.Error("the redirect target was requested")
		}
		http.Redirect(w, r, viaLocalhost+"/inside", http.StatusFound)
	}))
	defer srv.Close()
	viaLocalhost = localhostURL(t, srv)

	c := ConnectorFetch{
		Tokens: fakeTokenSource{token: "tok"}, Sessions: fakeSessionLookup{userID: uuid.New()},
		AllowedHosts: []string{"127.0.0.1"}, AllowPrivateNetworks: true,
	}
	in, _ := json.Marshal(map[string]string{"provider": "github", "url": srv.URL + "/start"})
	result, err := c.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Reason, "egress_denied") {
		t.Fatalf("Call() = %+v, want an egress_denied error", result)
	}
}

func TestEgressClient_StopsAfterFiveRedirects(t *testing.T) {
	chain := func(length int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/%d", &n)
			if n >= length {
				_, _ = w.Write([]byte("arrived"))
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/%d", n+1), http.StatusFound)
		}))
	}
	client := newEgressClient([]string{"127.0.0.1"}, true, 2*time.Second)

	five := chain(5)
	defer five.Close()
	resp, err := client.Get(five.URL + "/0") //nolint:noctx // a bounded test request
	if err != nil {
		t.Fatalf("five redirects should be followed: %v", err)
	}
	_ = resp.Body.Close()

	six := chain(6)
	defer six.Close()
	if resp, err := client.Get(six.URL + "/0"); err == nil { //nolint:noctx // a bounded test request
		_ = resp.Body.Close()
		t.Fatal("six chained redirects were followed")
	} else if !strings.Contains(err.Error(), "5 redirects") {
		t.Fatalf("error = %v, want the redirect limit named", err)
	}
}

func TestEgressClient_RefusesARedirectToANonHTTPScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()
	client := newEgressClient([]string{"*"}, true, 2*time.Second)
	if resp, err := client.Get(srv.URL); err == nil { //nolint:noctx // a bounded test request
		_ = resp.Body.Close()
		t.Fatal("a redirect to file:// was followed")
	} else if !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("error = %v, want the scheme named", err)
	}
}

// The transport must not use a proxy: its own resolution would sidestep the dialer.
func TestEgressClient_IgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	resp, err := newEgressClient([]string{"*"}, true, 2*time.Second).Get(srv.URL) //nolint:noctx // a bounded test request
	if err != nil {
		t.Fatalf("the request went through a proxy: %v", err)
	}
	_ = resp.Body.Close()
}
