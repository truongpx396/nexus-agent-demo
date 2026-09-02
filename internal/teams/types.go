// Package teams implements peer agent teams: shared task boards with
// Kanban-style claiming (README.md §9, Phase 9). New scope, not derived from
// the original's 67 patterns (README §3's own note) — it borrows every
// primitive it can from what already shipped (internal/queue's SKIP LOCKED
// claim, internal/delegate's copy-at-spawn/fold-at-boundary taint discipline
// and fan-out envelope, internal/memory's injection scanner) rather than
// inventing a second set of rules, and is bound by the same fail-closed,
// no-widening discipline as delegation (Phase 8).
//
// teams is free to import kernel and internal/store/internal/crypto/
// internal/audit directly — only kernel's own import allowlist is
// restricted (kernel/types.go); this mirrors internal/delegate's own package
// doc comment on the same point.
package teams

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status is the teams.status column's vocabulary (task 9.9).
type Status string

const (
	StatusActive           Status = "active"
	StatusCompleted        Status = "completed"
	StatusAborted          Status = "aborted"
	StatusCeilingExhausted Status = "ceiling_exhausted"
)

// CardStatus is the board_cards.status column's vocabulary (task 9.3).
type CardStatus string

const (
	CardOpen       CardStatus = "open"
	CardClaimed    CardStatus = "claimed"
	CardInProgress CardStatus = "in_progress"
	CardDone       CardStatus = "done"
	CardBlocked    CardStatus = "blocked"
)

// ScanStatus is the board_cards.injection_scan_status column's vocabulary
// (task 9.3) — deliberately only the two states write_card ever actually
// leaves a card in (clean/flagged): scanning is synchronous, the same
// honest simplification internal/memory.Screen already makes for a memory
// file. "pending" stays in the schema as a seam for a hypothetical future
// async scanner, exactly like tools.AdmissionPending is a seam no code path
// in this demo actually produces yet.
type ScanStatus string

const (
	ScanPending ScanStatus = "pending"
	ScanClean   ScanStatus = "clean"
	ScanFlagged ScanStatus = "flagged"
)

// MaxDepth mirrors internal/delegate.MaxDepth exactly (task 9.10: "no
// recursive teams, no depth workaround through a side door") — duplicated
// as a plain value, not imported, the same decoupling
// internal/tools/builtin/delegate.go's own maxDelegationDepth already
// chooses over importing internal/delegate for one constant. A session may
// create a team only if its OWN depth + 1 does not exceed this bound, which
// is also exactly the bound that already stops a depth-1 team member from
// then calling platform/delegate (internal/tools/builtin/delegate.go's own
// CheckPermissions re-derives depth independently and denies there too) —
// one shared number, enforced twice, never a single "is this a leaf" flag
// that a new call site could forget to check.
const MaxDepth = 1

// MemberSpec is one roster entry — an agent identity plus its own starting
// task, fixed at CreateTeam time (task 9.1's "no mid-run recruitment").
type MemberSpec struct {
	AgentID string
	Task    string
}

// CardSpec is one card CreateTeam seeds the board with, written under the
// COORDINATOR's own taint state (the same copy-at-write rule write_card
// enforces for every card written after the team exists) and never
// injection-scanned — first-party, operator-authored seed content sits in
// the same trust position a builtin tool's own descriptor does, never the
// less-trusted position a peer's own write_card call occupies.
type CardSpec struct {
	Title string
	Body  string
}

// Team mirrors one teams row.
type Team struct {
	TeamID               uuid.UUID
	TenantID             uuid.UUID
	Name                 string
	CoordinatorSessionID uuid.UUID
	Roster               []MemberSpec
	EnvelopeID           *uuid.UUID
	Status               Status
	Reason               string
	CreatedAt            time.Time
	CompletedAt          *time.Time
}

// Card mirrors one board_cards row.
type Card struct {
	CardID             uuid.UUID
	TenantID           uuid.UUID
	TeamID             uuid.UUID
	Title              string
	Body               string
	Status             CardStatus
	TaintState         [3]bool
	ScanStatus         ScanStatus
	ScanFindings       []string
	WrittenBySessionID uuid.UUID
	ClaimedBySessionID *uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func marshalRoster(roster []MemberSpec) ([]byte, error) { return json.Marshal(roster) }

func unmarshalRoster(b []byte) ([]MemberSpec, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []MemberSpec
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func marshalTaint(engaged [3]bool) ([]byte, error) { return json.Marshal(engaged) }

func unmarshalTaint(b []byte) ([3]bool, error) {
	var out [3]bool
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}
