package exechook

import (
	"context"
	"runtime"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
)

func TestHandler_RunsScriptAndParsesResponse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	h := Handler{}
	cfg := hooks.Config{
		Name:    "echo-ask",
		Kind:    hooks.KindCommand,
		Command: "/bin/sh",
		Args:    []string{"-c", `cat >/dev/null; echo '{"decision":"ask","reason":"confirm please"}'`},
	}
	out, err := h.Run(context.Background(), cfg, hooks.Context{ToolID: "platform/shell@v1", Namespace: "platform"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Decision != hooks.Ask || out.Reason != "confirm please" {
		t.Fatalf("out = %+v, want hooks.Decision=ask Reason=%q", out, "confirm please")
	}
}

func TestHandler_NonZeroExitIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	h := Handler{}
	cfg := hooks.Config{Name: "failing", Kind: hooks.KindCommand, Command: "/bin/sh", Args: []string{"-c", "exit 1"}}
	if _, err := h.Run(context.Background(), cfg, hooks.Context{ToolID: "x/y@v1"}); err == nil {
		t.Fatal("Run() = nil error, want an error for a non-zero exit")
	}
}
