package eventlog

import (
	"fmt"
	"sort"
)

// owners says which feature defines each event type: the package whose
// vocabulary gives the event its meaning and whose behaviour it records. The
// taxonomy stays one closed list in this package (event.go) and does not become
// a registry each feature adds to, for a reason that is about the core, not
// convenience: the kernel authors events from nearly every feature's vocabulary
// (it appends approval_requested, delegation_requested, budget_decision and
// taint_transition itself, and replays all of them to rebuild a run), so a type
// owned by the feature that defines it would still have to be named by the
// kernel, and an open registry would put kernel -> feature imports where there
// are none. What the closed list gives up in self-containment it returns as one
// place that says what any run's log can contain, which is what "reconstruct any
// run from the log alone" and the single upcasting path (upcast.go) rely on.
//
// The feature-shaped half is this table: every type names who owns it, a new
// type fails taxonomy_test.go until it is listed, and tests/contract checks each
// owner is a real package. A feature adds its event types here, in its own diff,
// beside its owner entry; it does not edit the kernel.
//
// Append refuses a type that is not in this table, so the list is closed at the
// write boundary and not only by convention.
var owners = map[EventType]string{
	// The run loop's own record.
	EventThought:       "kernel",
	EventContent:       "kernel",
	EventToolUse:       "kernel",
	EventToolResult:    "kernel",
	EventCondensation:  "kernel",
	EventContextPruned: "kernel",
	EventUserMessage:   "kernel",
	EventAwaitingInput: "kernel",
	EventError:         "kernel",
	EventTerminal:      "kernel",

	// Tools and the tool catalog.
	EventToolReceiptRef:       "internal/tools",
	EventToolLoaded:           "internal/tools",
	EventTaintTransition:      "internal/tools",
	EventSanitizationBoundary: "internal/tools",

	// Run control and reliability.
	EventEffectClaimed:       "internal/runctl",
	EventEffectClaimResolved: "internal/runctl",
	EventCheckpoint:          "internal/runctl",
	EventForked:              "internal/runctl",
	EventAutonomyTightened:   "internal/runctl",
	EventStuckSuspected:      "internal/reliability",

	// Human oversight.
	EventInputRequested:            "internal/oversight",
	EventInputAnswered:             "internal/oversight",
	EventInputExpired:              "internal/oversight",
	EventInputInvalidated:          "internal/oversight",
	EventApprovalRequested:         "internal/oversight",
	EventApprovalNotified:          "internal/oversight",
	EventApprovalReminded:          "internal/oversight",
	EventApprovalEscalated:         "internal/oversight",
	EventApprovalGranted:           "internal/oversight",
	EventApprovalGrantedModified:   "internal/oversight",
	EventApprovalDenied:            "internal/oversight",
	EventApprovalExpired:           "internal/oversight",
	EventApprovalInvalidated:       "internal/oversight",
	EventApprovalResolutionRefused: "internal/oversight",
	EventApprovalMismatch:          "internal/oversight",

	// Cost, memory, skills and the surfaces.
	EventBudgetDecision:         "internal/cost",
	EventMemoryLoaded:           "internal/memory",
	EventSkillActivated:         "internal/skills",
	EventSkillCapabilityIgnored: "internal/skills",
	EventDeliveryEnqueued:       "internal/surfaces",
	EventDeliveryDelivered:      "internal/surfaces",
	EventDeliveryFailed:         "internal/surfaces",
	EventDeliverySuppressed:     "internal/surfaces",

	// Delegation, plans and teams.
	EventDelegationRequested:      "internal/delegate",
	EventDelegationTargetSelected: "internal/delegate",
	EventDelegationRefused:        "internal/delegate",
	EventDelegationReturned:       "internal/delegate",
	EventDelegationReaped:         "internal/delegate",
	EventPlanStarted:              "internal/plan",
	EventPlanStepEntered:          "internal/plan",
	EventPlanTransition:           "internal/plan",
	EventPlanStepExited:           "internal/plan",
	EventPlanCompleted:            "internal/plan",
	EventTeamCreated:              "internal/teams",
	EventTeamEnded:                "internal/teams",
	EventBoardCardFlagged:         "internal/teams",

	// Content access and erasure.
	EventContentAccessGranted: "internal/contentaccess",
	EventContentAccessed:      "internal/contentaccess",
	EventContentAccessRefused: "internal/contentaccess",
	EventErasure:              "internal/crypto",
}

// ErrUnknownType is returned by an append of a type that is not in the taxonomy.
type ErrUnknownType struct{ Type EventType }

func (e ErrUnknownType) Error() string {
	return fmt.Sprintf("eventlog: event type %q is not in the taxonomy (add it to event.go and taxonomy.go)", string(e.Type))
}

// Known reports whether t is in the taxonomy. A log refuses to append anything
// else.
func (t EventType) Known() bool {
	_, ok := owners[t]
	return ok
}

// Owner is the package that defines t, as a path relative to the module root
// ("kernel", "internal/oversight"), or "" for a type outside the taxonomy.
func (t EventType) Owner() string { return owners[t] }

// Types returns every event type in the taxonomy, sorted.
func Types() []EventType {
	out := make([]EventType, 0, len(owners))
	for t := range owners {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
