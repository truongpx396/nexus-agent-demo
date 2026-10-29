package eventlog

// Session status values. SessionStatusSuspended is Phase 3's: the
// permission chain resolved ASK with no standing scope to satisfy it
// (kernel/loop.go's suspendForApproval), so the run pauses here rather than
// terminating or continuing. It is not a terminal status — Phase 5's
// internal/oversight turns the EventApprovalRequested that produced it into
// a real decision and, for that ONE pending tool_use, resumes the run via
// kernel.Kernel.Resume (README task 5.8). General crash/steer resume from
// an arbitrary point in a run is still Phase 6's internal/runctl + the real
// Checkpoint artifact — Phase 5's Resume is a narrower, honest interim
// scoped only to the approval-suspend case.
const (
	SessionStatusQueued    = "queued" // the schema default; a session that has never had Run() start
	SessionStatusRunning   = "running"
	SessionStatusSuspended = "suspended"
	SessionStatusCompleted = "completed"
	SessionStatusFailed    = "failed"
	// SessionStatusAwaitingInput is a conversational run's (kernel.
	// RunConfig.Conversational) own pause state: a plain content/empty
	// turn suspends here instead of terminating (kernel/terminal.go's
	// suspendForUserInput, EventAwaitingInput) — not terminal, and
	// deliberately distinct from SessionStatusSuspended (that one means
	// "a specific tool_use is waiting on a human decision"; this one
	// means "the model finished talking, ordinary conversational pause").
	// internal/runctl.Control.ResumeConversation is the only path back out
	// of it; cmd/nexusd's idle-conversation sweep is the backstop that
	// eventually ends one nobody ever answers.
	SessionStatusAwaitingInput = "awaiting_input"
)
