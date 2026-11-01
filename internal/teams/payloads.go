package teams

import "github.com/google/uuid"

type teamCreatedPayload struct {
	TeamID    uuid.UUID `json:"team_id"`
	Name      string    `json:"name"`
	Roster    []string  `json:"roster"` // agent_ids only — Task text is not audit-worthy metadata
	CardCount int       `json:"card_count"`
}

// teamEndedPayload is EventTeamEnded's sealed shape — appended onto the
// coordinator's own log exactly once, whichever of the three terminal
// statuses (task 9.9) fired. One event type carrying a typed Status field,
// not three near-duplicate event types, mirrors kernel/terminal.go's own
// EventTerminal/TerminalReason idiom.
type teamEndedPayload struct {
	TeamID uuid.UUID `json:"team_id"`
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
}

// boardCardFlaggedPayload is EventBoardCardFlagged's sealed shape —
// appended onto the WRITER's own log (task 9.7): a fail-closed marker,
// mirroring EventSkillCapabilityIgnored's own role of making a fail-closed
// decision visible in the audit trail rather than silent.
type boardCardFlaggedPayload struct {
	CardID   uuid.UUID `json:"card_id"`
	TeamID   uuid.UUID `json:"team_id"`
	Findings []string  `json:"findings,omitempty"`
}

// cardReadTaintTransitionPayload is EventTaintTransition's sealed shape for
// a READ-time fold (task 9.6) — the same event TYPE
// internal/delegate/resolve.go's own taintTransitionPayload already uses
// for a RETURN-time fold, carrying a different local payload shape (this
// codebase's own convention: each producer defines and reads back its own
// payload for the event types it writes; nothing reads taint_transition
// payloads generically across packages).
type cardReadTaintTransitionPayload struct {
	CardID  uuid.UUID `json:"card_id"`
	Engaged [3]bool   `json:"engaged"`
}
