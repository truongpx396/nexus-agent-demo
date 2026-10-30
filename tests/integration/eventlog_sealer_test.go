//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// sealerRig is a session whose log holds one really-encrypted event, which is
// what gives it an active key for the Sealer to find.
func sealerRig(t *testing.T) (*oversightRig, uuid.UUID, string) {
	t.Helper()
	r := setupOversightRig(t)
	sessionID := uuid.New()
	r.createSession(t, sessionID, uuid.New(), "autonomous")
	seal := r.seal(t, sessionID)
	var keyID string
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sealed, digest, kid, err := seal([]byte(`{"text":"hello"}`))
		if err != nil {
			return err
		}
		keyID = kid
		_, err = store.Append(ctx, tx, eventlog.Event{
			EventID: uuid.New(), SessionID: sessionID, TenantID: r.tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
			Type: eventlog.EventUserMessage, Payload: sealed, PayloadDigest: digest, KeyID: kid, Actor: eventlog.ActorUser,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return r, sessionID, keyID
}

func receiptCount(t *testing.T, r *oversightRig, sessionID uuid.UUID) int {
	t.Helper()
	var n int
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_receipts WHERE session_id = $1`, sessionID).Scan(&n)
	}); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	return n
}

func TestSealer_AppendsASealedChainedEventUnderTheSessionsKey(t *testing.T) {
	r, sessionID, keyID := sealerRig(t)
	ctx := context.Background()
	sealer := sealed.NewSealer(r.keys, r.chain)
	toolID, pair := "platform/shell@v1", uuid.New()
	before := receiptCount(t, r, sessionID)

	var got eventlog.Event
	err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		got, aerr = sealer.Bind(tx).Append(ctx, r.tenantID, sessionID, eventlog.EventApprovalGranted, &toolID, &pair, map[string]string{"decided_by": "alice"})
		return aerr
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if got.Type != eventlog.EventApprovalGranted || got.KeyID != keyID || got.Seq != 2 || got.Actor != eventlog.ActorSystem {
		t.Errorf("event = type %s key %s seq %d actor %s, want approval_granted under the session's key at seq 2, system-authored", got.Type, got.KeyID, got.Seq, got.Actor)
	}
	if got.ToolID == nil || *got.ToolID != toolID || got.PairRef == nil || *got.PairRef != pair {
		t.Errorf("tool id / pair ref = %v / %v, want %s / %s carried onto the event", got.ToolID, got.PairRef, toolID, pair)
	}

	// The stored payload is ciphertext that opens, under the session's key, to what was appended.
	events, err := listEventsDirect(ctx, r.st, r.tenantID, sessionID)
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %d, %v, want 2", len(events), err)
	}
	var dek crypto.DEK
	if err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = r.keys.Unwrap(ctx, tx, keyID)
		return uerr
	}); err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	plain, err := crypto.Open(dek, events[1].Payload, r.tenantID.String(), sessionID.String())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(plain, &body); err != nil || body["decided_by"] != "alice" {
		t.Errorf("payload = %s (%v), want the appended JSON", plain, err)
	}
	if string(events[1].Payload) == string(plain) {
		t.Error("the payload was stored in the clear")
	}

	if after := receiptCount(t, r, sessionID); after != before+1 {
		t.Errorf("audit receipts %d -> %d, want one more for the appended event", before, after)
	}
}

func TestSealer_ATrailingErasureRecordDoesNotChangeWhichKeySeals(t *testing.T) {
	r, sessionID, keyID := sealerRig(t)
	ctx := context.Background()
	if err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Append(ctx, tx, eventlog.Event{
			EventID: uuid.New(), SessionID: sessionID, TenantID: r.tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
			Type: eventlog.EventErasure, Payload: []byte(`{}`), PayloadDigest: []byte{0}, KeyID: crypto.ErasureKeyID, Actor: eventlog.ActorSystem,
		})
		return err
	}); err != nil {
		t.Fatalf("seed erasure record: %v", err)
	}

	sealer := sealed.NewSealer(r.keys, nil)
	var got eventlog.Event
	if err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		got, aerr = sealer.Bind(tx).Append(ctx, r.tenantID, sessionID, eventlog.EventApprovalGranted, nil, nil, struct{}{})
		return aerr
	}); err != nil {
		t.Fatalf("Append after an erasure record: %v", err)
	}
	if got.KeyID != keyID {
		t.Errorf("sealed under %q, want the session's real key %q: an erasure record's sentinel is plaintext, never a key", got.KeyID, keyID)
	}
}

func TestSealer_WithNoChainWritesNoReceipt(t *testing.T) {
	r, sessionID, _ := sealerRig(t)
	ctx := context.Background()
	before := receiptCount(t, r, sessionID)
	sealer := sealed.NewSealer(r.keys, nil)
	if err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := sealer.Bind(tx).Append(ctx, r.tenantID, sessionID, eventlog.EventApprovalGranted, nil, nil, struct{}{})
		return err
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if after := receiptCount(t, r, sessionID); after != before {
		t.Errorf("receipts %d -> %d with no chain wired, want unchanged", before, after)
	}
}

func TestSealer_AppendsWithTheCallersTransactionNotOutsideIt(t *testing.T) {
	r, sessionID, _ := sealerRig(t)
	ctx := context.Background()
	sealer := sealed.NewSealer(r.keys, r.chain)
	boom := errors.New("caller changed its mind")

	err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, aerr := sealer.Bind(tx).Append(ctx, r.tenantID, sessionID, eventlog.EventApprovalGranted, nil, nil, struct{}{}); aerr != nil {
			return aerr
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("transaction error = %v, want the caller's", err)
	}
	events, lerr := listEventsDirect(ctx, r.st, r.tenantID, sessionID)
	if lerr != nil || len(events) != 1 {
		t.Errorf("%d events after the caller rolled back (%v), want only the seed: a state change and its event commit together or not at all", len(events), lerr)
	}
}

func TestSealer_ASessionWithNoEventsHasNoKeyToSealUnderAndWritesNothing(t *testing.T) {
	r := setupOversightRig(t)
	ctx := context.Background()
	sessionID := uuid.New()
	r.createSession(t, sessionID, uuid.New(), "autonomous")
	sealer := sealed.NewSealer(r.keys, r.chain)

	err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, aerr := sealer.Bind(tx).Append(ctx, r.tenantID, sessionID, eventlog.EventApprovalGranted, nil, nil, struct{}{})
		return aerr
	})
	if err == nil {
		t.Fatal("Append succeeded for a session with no key to seal under")
	}
	if events, _ := listEventsDirect(ctx, r.st, r.tenantID, sessionID); len(events) != 0 {
		t.Errorf("%d events written for a failed Append", len(events))
	}
}
