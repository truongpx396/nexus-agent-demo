package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/contentaccess"
	restkit "github.com/truongpx396/nexus-agent/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent/internal/surfaces/rest/resttest"
)

// fakeStore is a GrantStore whose behaviour each test sets: the grants a
// principal holds, and which storage call fails.
type fakeStore struct {
	grants     []contentaccess.Grant
	issueErr   error
	listErr    error
	historyErr error
	history    []contentaccess.EventDTO // what History returns; one event when nil
	noHistory  bool                     // History returns nothing at all
}

func (f *fakeStore) Issue(_ context.Context, gr contentaccess.Grant) (contentaccess.Grant, error) {
	return gr, f.issueErr
}
func (f *fakeStore) ListFor(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]contentaccess.Grant, error) {
	return f.grants, f.listErr
}
func (f *fakeStore) RecordRefusal(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (f *fakeStore) History(context.Context, uuid.UUID, uuid.UUID) ([]contentaccess.EventDTO, error) {
	switch {
	case f.noHistory:
		return nil, f.historyErr
	case f.history != nil:
		return f.history, f.historyErr
	}
	return []contentaccess.EventDTO{{Seq: 1}}, f.historyErr
}
func (f *fakeStore) RecordAccess(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int) error {
	return nil
}

func serve(f *fakeStore) http.Handler {
	return restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: uuid.New()},
		Handler{Grants: contentaccess.NewGrants(f)}).Handler()
}

// A read is refused with 403 only when the principal holds no valid grant: that
// is the caller's to be told. A storage failure is a logged 500 that says nothing
// about the database; before this, both answered 403 with the cause in the body.
func TestReadUnderGrant_RefusesWithoutAGrantAndNeverEchoesAStorageError(t *testing.T) {
	path := "/v1/sessions/" + uuid.New().String() + "/content-access-grants/read"
	valid := []contentaccess.Grant{{ExpiresAt: time.Now().Add(time.Hour)}}

	if rec := resttest.Do(t, serve(&fakeStore{grants: valid}), http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Errorf("a read under a valid grant = %d (%s), want 200", rec.Code, rec.Body)
	}
	if rec := resttest.Do(t, serve(&fakeStore{}), http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a read with no grant = %d, want 403", rec.Code)
	}
	for name, f := range map[string]*fakeStore{
		"lookup":  {listErr: errors.New("dial tcp 10.0.0.7:5432: refused")},
		"decrypt": {grants: valid, historyErr: errors.New("kms 10.0.0.7: timeout")},
	} {
		rec := resttest.Do(t, serve(f), http.MethodGet, path, "")
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.7") {
			t.Errorf("%s failure = %d %q, want a 500 that omits the cause", name, rec.Code, rec.Body)
		}
	}
}

func TestRequestGrant_ANonStorageErrorIsNotEchoed(t *testing.T) {
	path := "/v1/sessions/" + uuid.New().String() + "/content-access-grants"
	body := `{"grantee_id":"` + uuid.New().String() + `","reason":"support ticket"}`

	if rec := resttest.Do(t, serve(&fakeStore{}), http.MethodPost, path, body); rec.Code != http.StatusCreated {
		t.Fatalf("a grant = %d (%s), want 201", rec.Code, rec.Body)
	}
	rec := resttest.Do(t, serve(&fakeStore{issueErr: errors.New("pq: connection 10.0.0.7 refused")}), http.MethodPost, path, body)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("a failed grant = %d %q, want a 500 that omits the cause", rec.Code, rec.Body)
	}
}

// The events come back under the capitalised keys contentaccess.EventDTO has
// always been encoded with (api/openapi.yaml's GrantedEvent records that), and an
// empty history is an array a client can iterate, not null.
func TestReadUnderGrant_RendersEventsUnderTheirDocumentedKeysAndAnEmptyHistoryAsAnArray(t *testing.T) {
	path := "/v1/sessions/" + uuid.New().String() + "/content-access-grants/read"
	valid := []contentaccess.Grant{{ExpiresAt: time.Now().Add(time.Hour)}}
	id, at := uuid.New(), time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	rec := resttest.Do(t, serve(&fakeStore{grants: valid, history: []contentaccess.EventDTO{
		{EventID: id, Seq: 3, Type: "content", Body: json.RawMessage(`{"text":"hi"}`), CreatedAt: at},
		{Seq: 4, Type: "thought"},
	}}), http.MethodGet, path, "")
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil || len(events) != 2 {
		t.Fatalf("body %q: %v, want two events", rec.Body, err)
	}
	for _, key := range []string{"EventID", "Seq", "Type", "Body", "CreatedAt"} {
		if _, ok := events[0][key]; !ok {
			t.Errorf("event has no %q key: %q", key, events[0])
		}
	}
	if got := string(events[0]["EventID"]); got != `"`+id.String()+`"` {
		t.Errorf("EventID = %s, want %q", got, id)
	}
	if got := string(events[0]["Body"]); got != `{"text":"hi"}` {
		t.Errorf("Body = %s, want the decrypted payload as it is", got)
	}
	if got := string(events[1]["Body"]); got != "null" {
		t.Errorf("a missing Body = %s, want null", got)
	}

	empty := resttest.Do(t, serve(&fakeStore{grants: valid, noHistory: true}), http.MethodGet, path, "")
	if got := strings.TrimSpace(empty.Body.String()); got != "[]" {
		t.Errorf("an empty history rendered %q, want []", got)
	}
}

func TestRequestGrant_AnswersTheGrantAndRefusesWhatItCannotAct(t *testing.T) {
	session, grantee := uuid.New(), uuid.New()
	path := "/v1/sessions/" + session.String() + "/content-access-grants"

	rec := resttest.Do(t, serve(&fakeStore{}), http.MethodPost, path, `{"grantee_id":"`+grantee.String()+`","reason":"support ticket","ttl_seconds":60}`)
	var grant struct {
		GrantID   uuid.UUID `json:"grant_id"`
		SessionID uuid.UUID `json:"session_id"`
		GranteeID uuid.UUID `json:"grantee_id"`
		Reason    string    `json:"reason"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("a grant = %d %q (%v), want 201 and the grant", rec.Code, rec.Body, err)
	}
	if grant.SessionID != session || grant.GranteeID != grantee || grant.Reason != "support ticket" || grant.GrantID == uuid.Nil || !grant.ExpiresAt.After(time.Now()) {
		t.Errorf("grant = %+v, want the session, grantee and reason asked for, a grant id and an expiry ahead", grant)
	}

	for name, tc := range map[string]struct{ path, body string }{
		"no grantee_id":                   {path, `{"reason":"x"}`},
		"a grantee_id that is not a uuid": {path, `{"grantee_id":"nobody","reason":"x"}`},
		"no reason":                       {path, `{"grantee_id":"` + grantee.String() + `"}`},
		"a session id that is not a uuid": {"/v1/sessions/nope/content-access-grants", `{"grantee_id":"` + grantee.String() + `","reason":"x"}`},
	} {
		if rec := resttest.Do(t, serve(&fakeStore{}), http.MethodPost, tc.path, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d (%s), want 400", name, rec.Code, rec.Body)
		}
	}
}
