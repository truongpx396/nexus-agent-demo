package hooks

import (
	"context"
	"runtime"
	"testing"
)

func TestCommandHandler_RunsScriptAndParsesResponse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	h := CommandHandler{}
	cfg := Config{
		Name:    "echo-ask",
		Kind:    KindCommand,
		Command: "/bin/sh",
		Args:    []string{"-c", `cat >/dev/null; echo '{"decision":"ask","reason":"confirm please"}'`},
	}
	out, err := h.Run(context.Background(), cfg, Context{ToolID: "platform/shell@v1", Namespace: "platform"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Decision != Ask || out.Reason != "confirm please" {
		t.Fatalf("out = %+v, want Decision=ask Reason=%q", out, "confirm please")
	}
}

func TestCommandHandler_NonZeroExitIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	h := CommandHandler{}
	cfg := Config{Name: "failing", Kind: KindCommand, Command: "/bin/sh", Args: []string{"-c", "exit 1"}}
	if _, err := h.Run(context.Background(), cfg, Context{ToolID: "x/y@v1"}); err == nil {
		t.Fatal("Run() = nil error, want an error for a non-zero exit")
	}
}
