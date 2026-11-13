package main

import (
	"strings"
	"testing"
)

func TestRedisOptions_ATLSURLCarriesTheTLSConfigTheUserAndThePassword(t *testing.T) {
	opts, err := redisOptions("rediss://nexus:s3cret@redis.internal:6380/2", "ignored:6379")
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("a rediss:// URL produced no TLS config: the connection would be plaintext")
	}
	if opts.TLSConfig.ServerName != "redis.internal" {
		t.Errorf("TLS server name = %q, want the host, so the certificate is checked against it", opts.TLSConfig.ServerName)
	}
	if opts.Username != "nexus" || opts.Password != "s3cret" || opts.Addr != "redis.internal:6380" || opts.DB != 2 {
		t.Errorf("options = user %q password %q addr %q db %d", opts.Username, opts.Password, opts.Addr, opts.DB)
	}
}

func TestRedisOptions_APlainURLHasAPasswordAndNoTLS(t *testing.T) {
	opts, err := redisOptions("redis://:pw@localhost:6380/0", "")
	if err != nil {
		t.Fatal(err)
	}
	if opts.TLSConfig != nil || opts.Password != "pw" || opts.Addr != "localhost:6380" {
		t.Errorf("options = %+v", opts)
	}
}

// With only the address it is today's client: no password, no TLS.
func TestRedisOptions_OnlyAnAddressIsTodaysOptions(t *testing.T) {
	opts, err := redisOptions("", "localhost:6380")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Addr != "localhost:6380" || opts.TLSConfig != nil || opts.Password != "" || opts.Username != "" {
		t.Errorf("options = %+v", opts)
	}
}

// The URL wins when both are set.
func TestRedisOptions_TheURLWinsOverTheAddress(t *testing.T) {
	opts, err := redisOptions("rediss://redis.internal:6380/0", "localhost:6380")
	if err != nil || opts.Addr != "redis.internal:6380" {
		t.Fatalf("options = %+v, %v", opts, err)
	}
}

// A malformed URL is an error that does not repeat the URL, which has a password in it.
func TestRedisOptions_AMalformedURLIsAnErrorThatDoesNotLeakTheCredential(t *testing.T) {
	for name, url := range map[string]string{ //nolint:gosec // URL fixtures, not credentials
		"wrong scheme":   "http://user:hunter2@host:6380/0",
		"not a URL":      "user:hunter2@host:6380",
		"a bad port":     "rediss://user:hunter2@host:notaport/0",
		"a bad database": "redis://user:hunter2@host:6380/notanumber",
	} {
		_, err := redisOptions(url, "localhost:6380")
		if err == nil {
			t.Errorf("%s: %q was accepted", name, url)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), url) {
			t.Errorf("%s: the error %q repeats the URL", name, err)
		}
		if !strings.Contains(err.Error(), "NEXUS_REDIS_URL") {
			t.Errorf("%s: the error %q does not name the variable", name, err)
		}
	}
}

func TestNewRedisClient_ReadsTheEnvironment(t *testing.T) {
	t.Setenv("NEXUS_REDIS_URL", "rediss://nexus:pw@redis.internal:6380/0")
	t.Setenv("NEXUS_REDIS_ADDR", "")
	c, err := newRedisClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck // a client that never connected
	if got := c.Options(); got.Addr != "redis.internal:6380" || got.TLSConfig == nil {
		t.Errorf("client options = %+v", got)
	}

	t.Setenv("NEXUS_REDIS_URL", "")
	c2, err := newRedisClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close() //nolint:errcheck // a client that never connected
	if got := c2.Options().Addr; got != defaultRedisAddr {
		t.Errorf("with neither set the address is %q, want the local default %q", got, defaultRedisAddr)
	}

	t.Setenv("NEXUS_REDIS_URL", "not a url")
	if _, err := newRedisClient(); err == nil {
		t.Error("a malformed NEXUS_REDIS_URL was accepted")
	}
}
