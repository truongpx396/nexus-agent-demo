package main

import (
	"slices"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args []string
		name string
		rest []string
		dev  bool
	}{
		{nil, "serve", nil, false},
		{[]string{"--dev"}, "serve", nil, true},
		{[]string{"migrate"}, "migrate", nil, false},
		{[]string{"--dev", "seed", "--tenant", "acme"}, "seed", []string{"--tenant", "acme"}, true},
		{[]string{"token", "--dev", "--user", "u1"}, "token", []string{"--user", "u1"}, true},
	}
	for _, c := range cases {
		name, rest, dev := parseArgs(c.args)
		if name != c.name || dev != c.dev || !slices.Equal(rest, c.rest) {
			t.Errorf("parseArgs(%q) = %q %q dev=%v, want %q %q dev=%v", c.args, name, rest, dev, c.name, c.rest, c.dev)
		}
	}
}

// A mistyped subcommand must exit non-zero rather than boot a server, and every
// documented command must be in the table the usage text is derived from.
func TestRun_RefusesAnUnknownCommand(t *testing.T) {
	if code := run([]string{"serv"}); code != 2 {
		t.Errorf("run(serv) = %d, want 2", code)
	}
	for _, name := range []string{"serve", "migrate", "seed", "verify-chain", "dashboard", "go-live", "erase", "ingest", "token"} {
		if commands[name] == nil {
			t.Errorf("command %q is not registered", name)
		}
	}
}
