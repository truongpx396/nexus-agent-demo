package store

import "encoding/json"

// Projection is the read path Snapshot (snapshot.go) caches and
// ReplayProjection derives from scratch — task 6.4's own acceptance test
// exists to prove these two routes to a Projection always agree.
type Projection struct {
	Status         string
	TerminalReason *string
}

// ReplayProjection derives a session's status PURELY from event TYPES and
// PairRefs — no decrypt, mirroring kernel.Hygiene's own "structural only"
// discipline — proving status is a genuine, independently-rebuildable
// projection of the append-only log, never a second source of truth
// (Principle II; store/session.go's own doc comment on the sessions.status
// column, which this function reimplements as a pure replay instead of a
// same-transaction write). It intentionally cannot recover the PRECISE
// terminal reason string (that lives inside the sealed EventTerminal
// payload, kernel/terminal.go's terminalEventPayload) — only whether the
// run ended at all; ReplayFullProjection layers a decrypt of that one event
// on top when the exact reason is needed. history must be in seq order
// (store.ListEvents' own contract).
func ReplayProjection(history []Event) Projection {
	status := SessionStatusQueued
	for _, e := range history {
		switch e.Type { //nolint:exhaustive // deliberately narrow: only the event types that ever move a session between queued/running/suspended/ended are structurally relevant here; every other type (thought, budget_decision, tool_loaded, ...) leaves status unchanged
		case EventUserMessage, EventToolUse, EventToolResult, EventContent, EventThought:
			if status != SessionStatusSuspended {
				status = SessionStatusRunning
			}
		case EventApprovalRequested, EventInputRequested:
			status = SessionStatusSuspended
		case EventApprovalGranted, EventApprovalGrantedModified, EventApprovalDenied,
			EventApprovalInvalidated, EventInputAnswered, EventInputExpired, EventInputInvalidated:
			status = SessionStatusRunning
		case EventTerminal:
			// Which of completed/failed it was needs the sealed payload
			// (ReplayFullProjection); structurally we only know it's over.
			status = SessionStatusCompleted
		}
	}
	return Projection{Status: status}
}

// TerminalDecryptFunc mirrors kernel.DecryptFunc's shape exactly (a local,
// structurally-identical declaration rather than an import — this package
// cannot import internal/crypto without a cycle, since internal/crypto
// already imports internal/store; see crypto/shred.go's own
// appendErasureEvent) so ReplayFullProjection stays free of any crypto
// dependency of its own.
type TerminalDecryptFunc func(e Event) (plaintext []byte, err error)

// terminalPayload mirrors kernel/terminal.go's unexported
// terminalEventPayload field-for-field — duplicated for the same reason
// oversight/invalidate.go duplicates kernel's toolResultPayload shape: this
// package has no access to kernel's unexported type and no business
// importing kernel to get it (kernel imports store, never the reverse).
type terminalPayload struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// ReplayFullProjection is ReplayProjection plus ONE selective decrypt (the
// last EventTerminal, if any) to recover the precise terminal reason —
// exactly the layering kernel.Rehydrate already uses (structural replay,
// selective decrypt), never a full-history decrypt just to answer "how did
// this end."
func ReplayFullProjection(history []Event, decrypt TerminalDecryptFunc) (Projection, error) {
	p := ReplayProjection(history)
	if p.Status != SessionStatusCompleted {
		return p, nil
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Type != EventTerminal {
			continue
		}
		plaintext, err := decrypt(history[i])
		if err != nil {
			return Projection{}, err
		}
		var tp terminalPayload
		if err := json.Unmarshal(plaintext, &tp); err != nil {
			return Projection{}, err
		}
		if tp.Reason != "completed" {
			p.Status = SessionStatusFailed
		}
		reason := tp.Reason
		p.TerminalReason = &reason
		break
	}
	return p, nil
}
