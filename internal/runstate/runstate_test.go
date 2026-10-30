package runstate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runstate"
)

var tenant = uuid.New()

// fakeUnit is a Unit over one session's rows and a key table, counting how
// often a key is unwrapped.
type fakeUnit struct {
	sessions map[uuid.UUID]eventlog.Session
	events   map[uuid.UUID][]eventlog.Event
	keys     map[string]crypto.DEK
	unwraps  int
}

func (u *fakeUnit) GetSession(_ context.Context, id uuid.UUID) (eventlog.Session, error) {
	s, ok := u.sessions[id]
	if !ok {
		return eventlog.Session{}, errors.New("no such session")
	}
	return s, nil
}
func (u *fakeUnit) CreateSession(context.Context, eventlog.Session) error { return nil }
func (u *fakeUnit) SetSessionStatus(context.Context, uuid.UUID, string, *string) error {
	return nil
}
func (u *fakeUnit) ListEvents(_ context.Context, id uuid.UUID) ([]eventlog.Event, error) {
	return u.events[id], nil
}
func (u *fakeUnit) NewDEK(context.Context, uuid.UUID) (crypto.DEK, error) {
	return crypto.GenerateDEK("minted")
}
func (u *fakeUnit) Unwrap(_ context.Context, keyID string) (crypto.DEK, error) {
	u.unwraps++
	dek, ok := u.keys[keyID]
	if !ok {
		return crypto.DEK{}, errors.New("no such key")
	}
	return dek, nil
}

type fakeStore struct{ u *fakeUnit }

func (s fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(ctx context.Context, tx *fakeUnit) error) error {
	return fn(ctx, s.u)
}

// userMessage is a sealed user_message event of sessionID under dek.
func userMessage(t *testing.T, dek crypto.DEK, sessionID uuid.UUID, body string) eventlog.Event {
	t.Helper()
	plaintext, _ := json.Marshal(map[string]string{"body": body})
	sealed, err := crypto.Seal(dek, plaintext, tenant.String(), sessionID.String())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return eventlog.Event{EventID: uuid.New(), SessionID: sessionID, TenantID: tenant, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: eventlog.EventUserMessage, Payload: sealed, KeyID: dek.KeyID}
}

func newSession(t *testing.T, bodies ...string) (fakeStore, *fakeUnit, uuid.UUID, crypto.DEK) {
	t.Helper()
	dek, err := crypto.GenerateDEK("k1")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	id := uuid.New()
	u := &fakeUnit{
		sessions: map[uuid.UUID]eventlog.Session{id: {SessionID: id, AutonomyLevel: "supervised", Status: eventlog.SessionStatusSuspended}},
		events:   map[uuid.UUID][]eventlog.Event{}, keys: map[string]crypto.DEK{"k1": dek},
	}
	for _, b := range bodies {
		u.events[id] = append(u.events[id], userMessage(t, dek, id, b))
	}
	return fakeStore{u}, u, id, dek
}

func TestLoad_RebuildsTheTranscriptAndSealsUnderTheSessionsKey(t *testing.T) {
	st, _, id, dek := newSession(t, "first", "second")

	got, err := runstate.Load[*fakeUnit](context.Background(), st, tenant, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Session.AutonomyLevel != "supervised" || len(got.History) != 2 {
		t.Errorf("session = %+v history %d, want the session and its two events", got.Session, len(got.History))
	}
	if n := len(got.State.Transcript); n != 2 || got.State.Transcript[1].Blocks[0].Text != "second" {
		t.Errorf("transcript has %d messages, want the two user messages in order", n)
	}

	// What the resumed run seals must open under the session's own key, bound to
	// this tenant and session.
	sealed, digest, keyID, err := got.State.Seal([]byte(`{"body":"resumed"}`))
	if err != nil || keyID != dek.KeyID || len(digest) == 0 {
		t.Fatalf("Seal = key %q digest %d bytes, %v", keyID, len(digest), err)
	}
	if plain, err := crypto.Open(dek, sealed, tenant.String(), id.String()); err != nil || string(plain) != `{"body":"resumed"}` {
		t.Errorf("sealed payload opens to %q, %v, want the original under the session's key", plain, err)
	}
	if _, err := crypto.Open(dek, sealed, tenant.String(), uuid.NewString()); err == nil {
		t.Error("the payload opens under another session id: it must be bound to its own session")
	}
}

func TestLoad_UnwrapsEachKeyOnceHoweverManyEventsItSeals(t *testing.T) {
	st, u, id, _ := newSession(t, "a", "b", "c", "d")
	if _, err := runstate.Load[*fakeUnit](context.Background(), st, tenant, id); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// One unwrap to decrypt the history (cached across its four events) and one
	// for the key the resumed run will seal under.
	if u.unwraps != 2 {
		t.Errorf("keys unwrapped %d times, want 2: decrypting each event separately would hit the key store per event", u.unwraps)
	}
}

func TestActiveKeyID_IsTheMostRecentRealKey(t *testing.T) {
	got, err := runstate.ActiveKeyID([]eventlog.Event{{KeyID: "key-1"}, {KeyID: "key-2"}})
	if err != nil || got != "key-2" {
		t.Errorf("ActiveKeyID = %q, %v, want key-2", got, err)
	}
}

func TestActiveKeyID_SkipsAnErasureRecord(t *testing.T) {
	_, _, id, dek := newSession(t)
	history := []eventlog.Event{
		userMessage(t, dek, id, "x"),
		{Type: eventlog.EventTerminal, KeyID: crypto.ErasureKeyID}, // plaintext, never a real key
	}
	if got, err := runstate.ActiveKeyID(history); err != nil || got != "k1" {
		t.Errorf("ActiveKeyID = %q, %v, want the last real key k1", got, err)
	}
	if _, err := runstate.ActiveKeyID([]eventlog.Event{{KeyID: crypto.ErasureKeyID}}); err == nil {
		t.Error("ActiveKeyID found a key in a history that has only an erasure record")
	}
}

func TestLoad_FailsClosedWithoutAnActiveKeyOrASession(t *testing.T) {
	st, _, id, _ := newSession(t) // a session with no events at all
	if _, err := runstate.Load[*fakeUnit](context.Background(), st, tenant, id); err == nil {
		t.Error("Load of a session with no sealed history succeeded: there is no key to seal the resumed run under")
	}
	if _, err := runstate.Load[*fakeUnit](context.Background(), st, tenant, uuid.New()); err == nil {
		t.Error("Load of a session that does not exist succeeded")
	}
}

func TestDecryptor_PassesAnErasureRecordThroughUntouched(t *testing.T) {
	st, _, id, _ := newSession(t)
	dec := runstate.Decryptor[*fakeUnit](st, tenant, id)
	got, err := dec(context.Background(), eventlog.Event{KeyID: crypto.ErasureKeyID, Payload: []byte("erased")})
	if err != nil || string(got) != "erased" {
		t.Errorf("decrypt of an erasure record = %q, %v, want the plaintext as is", got, err)
	}
}

// TestSeal_RoundTrips is README task 13.8: runstate.Seal is what every
// resumed or spawned run seals its events with — a fast, in-memory
// round trip (no DB, no KeyStore) over the same crypto.Seal/Open pair the
// real DEK-backed path uses, proving the closure it returns actually seals
// what it's given and reports a stable digest/key id.
func TestSeal_RoundTrips(t *testing.T) {
	dek, err := crypto.GenerateDEK("test-key-1")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	tenantID, sessionID := uuid.New(), uuid.New()
	seal := runstate.Seal(dek, tenantID, sessionID)

	plaintext := []byte(`{"tool_name":"delegate"}`)
	sealed, digest, keyID, err := seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if keyID != dek.KeyID {
		t.Errorf("keyID = %q, want %q", keyID, dek.KeyID)
	}
	if !bytes.Equal(digest, crypto.Digest(plaintext)) {
		t.Error("digest does not match crypto.Digest(plaintext)")
	}

	opened, err := crypto.Open(dek, sealed, tenantID.String(), sessionID.String())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Errorf("round trip = %q, want %q", opened, plaintext)
	}

	// Sealing under the WRONG tenant/session (the AAD) must fail to open —
	// this is what actually scopes an event to its own session.
	if _, err := crypto.Open(dek, sealed, uuid.New().String(), sessionID.String()); err == nil {
		t.Error("Open succeeded with the wrong tenantID AAD — sealed data is not scoped correctly")
	}
}
