package sandbox

import "context"

// SessionSandbox binds a Docker backend to one session's Config (in
// particular, its own WorkspaceDir) and implements the small structural
// interface internal/tools.RunContext.Sandbox expects — internal/sandbox
// has no reason to import internal/tools just to name that interface; Go's
// structural typing satisfies it without either package knowing about the
// other's declaration, the same trick kernel.SealFunc and internal/
// surfaces/rest.SealFunc already use for two independently-declared,
// structurally-identical function types.
type SessionSandbox struct {
	Docker *Docker
	Config Config
}

// Exec runs cmd to completion and flattens sandbox.Result into the plain
// return values internal/tools.SandboxExec declares — that interface has no
// reason to know about this package's own Result/BreachKind types.
func (s SessionSandbox) Exec(ctx context.Context, cmd string) (output string, exitCode int, breach string, err error) {
	res, err := s.Docker.Exec(ctx, s.Config, cmd)
	if err != nil {
		return "", 0, "", err
	}
	return res.Output, res.ExitCode, string(res.Breach), nil
}
