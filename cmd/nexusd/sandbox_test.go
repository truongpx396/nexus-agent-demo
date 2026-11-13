package main

import (
	"strings"
	"testing"
)

// Outside --dev a deployment must name a sandbox: with none, platform/shell runs
// commands on the host as the nexusd user.
func TestNewSandboxFactory_FailsClosedOutsideDev(t *testing.T) {
	for name, tc := range map[string]struct {
		sandbox string
		url     string
		want    string // a substring of the error
	}{
		"unset":                      {"", "", "NEXUS_SANDBOX must be"},
		"a value that is not a kind": {"none", "", "NEXUS_SANDBOX must be"},
		"opensandbox with no URL":    {"opensandbox", "", "NEXUS_OPENSANDBOX_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("NEXUS_SANDBOX", tc.sandbox)
			t.Setenv("NEXUS_OPENSANDBOX_URL", tc.url)
			factory, err := newSandboxFactory(t.TempDir(), false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newSandboxFactory(dev=false) = %v, %v, want an error mentioning %q", factory != nil, err, tc.want)
			}
			if factory != nil {
				t.Fatal("a failed factory must not be usable")
			}
		})
	}

	t.Run("the error names the variable and both accepted values", func(t *testing.T) {
		t.Setenv("NEXUS_SANDBOX", "")
		_, err := newSandboxFactory(t.TempDir(), false)
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"NEXUS_SANDBOX", `"docker"`, `"opensandbox"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
	})
}

func TestNewSandboxFactory_OutsideDevAcceptsAConfiguredBackend(t *testing.T) {
	t.Setenv("NEXUS_SANDBOX", "opensandbox")
	t.Setenv("NEXUS_OPENSANDBOX_URL", "http://localhost:8090")
	factory, err := newSandboxFactory(t.TempDir(), false)
	if err != nil || factory == nil {
		t.Fatalf("newSandboxFactory = %v, %v, want a factory", factory != nil, err)
	}

	// Docker's client is built without contacting the daemon, so a configured
	// docker sandbox starts; a down daemon fails each shell call, never runs it on the host.
	t.Setenv("NEXUS_SANDBOX", "docker")
	factory, err = newSandboxFactory(t.TempDir(), false)
	if err != nil || factory == nil {
		t.Fatalf("newSandboxFactory(docker) = %v, %v, want a factory", factory != nil, err)
	}
}

// --dev keeps the zero-setup path: no Docker daemon, no OpenSandbox server.
func TestNewSandboxFactory_DevKeepsTheUnsandboxedFallback(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":                   {"NEXUS_SANDBOX": ""},
		"opensandbox with no URL": {"NEXUS_SANDBOX": "opensandbox", "NEXUS_OPENSANDBOX_URL": ""},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			factory, err := newSandboxFactory(t.TempDir(), true)
			if err != nil || factory != nil {
				t.Fatalf("newSandboxFactory(dev=true) = %v, %v, want (nil, nil)", factory != nil, err)
			}
		})
	}
}
