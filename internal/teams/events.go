package teams

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// deps is the small set of collaborators every teams component needs —
// mirrors internal/delegate's own unexported deps type field-for-field, for
// the same reason that file's own doc comment gives.
type deps struct {
	Store *store.Store
	Keys  *crypto.KeyStore
	Chain *audit.Chain
}

func activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var keyID string
	err := tx.QueryRow(ctx,
		`SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, crypto.ErasureKeyID,
	).Scan(&keyID)
	if err != nil {
		return "", fmt.Errorf("teams: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

// appendEvent seals payload under sessionID's active DEK and durably
// appends it — the marshal -> seal -> append -> chain sequence
// internal/delegate/events.go's own appendEvent already runs, reimplemented
// here for the same out-of-band reason that file's doc comment gives: this
// package drives events onto a session's log from OUTSIDE any live
// kernel.Run call, with no RunState.Seal closure to reuse.
func (d deps) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, toolID *string, pairRef *uuid.UUID, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("teams: marshal %s payload: %w", typ, err)
	}
	keyID, err := activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return store.Event{}, err
	}
	dek, err := d.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return store.Event{}, fmt.Errorf("teams: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return store.Event{}, fmt.Errorf("teams: seal %s payload: %w", typ, err)
	}

	e := store.Event{
		EventID:       uuid.New(),
		SessionID:     sessionID,
		TenantID:      tenantID,
		SchemaVersion: store.CurrentSchemaVersion,
		Type:          typ,
		Payload:       sealed,
		PayloadDigest: crypto.Digest(plaintext),
		KeyID:         dek.KeyID,
		Actor:         store.ActorSystem,
		ToolID:        toolID,
		PairRef:       pairRef,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return store.Event{}, fmt.Errorf("teams: append %s: %w", typ, err)
	}
	if d.Chain != nil {
		if _, err := d.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("teams: chain %s: %w", typ, err)
		}
	}
	return out, nil
}

// teamCreatedPayload is EventTeamCreated's sealed shape — appended onto the
// COORDINATOR's own log at CreateTeam time, mirroring how
// EventPlanStarted/EventDelegationRequested each land on the session that
// initiated the thing they describe.
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
