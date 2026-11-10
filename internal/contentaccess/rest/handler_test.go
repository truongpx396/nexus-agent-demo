package rest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/contentaccess"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// fakeStore is a GrantStore whose behaviour each test sets: the grants a
// principal holds, and which storage call fails.
type fakeStore struct {
	grants     []contentaccess.Grant
	issueErr   error
	listErr    error
	historyErr error
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
