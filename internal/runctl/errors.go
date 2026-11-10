package runctl

import (
	"errors"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// The errors below are the refusals a caller can act on. Each operation wraps
// one with %w around the specific message, so a binding tells "your request
// conflicts with the session" from "the database failed" with errors.Is rather
// than by status code or by reading text: the first is the caller's to be told
// (404, 409, 400), the second is an infrastructure failure to log and not echo.
var (
	// ErrSessionNotFound reports a session that does not exist or is not visible
	// to the tenant. It is eventlog.ErrSessionNotFound, re-exported so a binding
	// needs only this package.
	ErrSessionNotFound = eventlog.ErrSessionNotFound

	// ErrSessionState reports an operation the session's current status refuses:
	// steering a finished run, resuming a conversation that is not awaiting input.
	ErrSessionState = errors.New("runctl: session in wrong state")

	// ErrInvalidArgument reports an argument the operation cannot act on: an
	// unknown or widening autonomy target, a fork point with no events.
	ErrInvalidArgument = errors.New("runctl: invalid argument")
)
