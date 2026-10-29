package obs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/obs"
)

// fakeGrantStore records what Grants asks of storage and answers from fields.
type fakeGrantStore struct {
	grants  []obs.Grant // what ListFor returns, newest first
	history []obs.EventDTO

	issued   []obs.Grant
	refusals []string
	accesses []int // event counts recorded
	listed   int
	opened   int // History calls

	issueErr, listErr, refusalErr, historyErr, accessErr error
}

func (f *fakeGrantStore) Issue(_ context.Context, gr obs.Grant) (obs.Grant, error) {
	if f.issueErr != nil {
		return obs.Grant{}, f.issueErr
	}
	f.issued = append(f.issued, gr)
	return gr, nil
}

func (f *fakeGrantStore) ListFor(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]obs.Grant, error) {
	f.listed++
	return f.grants, f.listErr
}

func (f *fakeGrantStore) RecordRefusal(_ context.Context, _, _, _ uuid.UUID, reason string) error {
	if f.refusalErr != nil {
		return f.refusalErr
	}
	f.refusals = append(f.refusals, reason)
	return nil
}

func (f *fakeGrantStore) History(context.Context, uuid.UUID, uuid.UUID) ([]obs.EventDTO, error) {
	f.opened++
	return f.history, f.historyErr
}

func (f *fakeGrantStore) RecordAccess(_ context.Context, _, _, _ uuid.UUID, n int) error {
	if f.accessErr != nil {
		return f.accessErr
	}
	f.accesses = append(f.accesses, n)
	return nil
}

var (
	t0      = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	tenant  = uuid.New()
	session = uuid.New()
	grantee = uuid.New()
)

func newGrants(f *fakeGrantStore) *obs.Grants {
	g := obs.NewGrants(f)
	g.Now = func() time.Time { return t0 }
	return g
}

func grant(expires time.Time, revoked *time.Time) obs.Grant {
	return obs.Grant{GrantID: uuid.New(), TenantID: tenant, SessionID: session, GranteeID: grantee, ExpiresAt: expires, RevokedAt: revoked}
}

func TestRequestGrant_DefaultsToAnHourAndRecordsWhatWasAsked(t *testing.T) {
	f := &fakeGrantStore{}
	got, err := newGrants(f).RequestGrant(context.Background(), tenant, session, grantee, "support ticket 4411", 0)
	if err != nil {
		t.Fatalf("RequestGrant: %v", err)
	}
	if !got.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("a grant with no ttl expires %v, want an hour from now (%v)", got.ExpiresAt, t0.Add(time.Hour))
	}
	if len(f.issued) != 1 || f.issued[0].Reason != "support ticket 4411" || f.issued[0].GranteeID != grantee || f.issued[0].SessionID != session || f.issued[0].TenantID != tenant {
		t.Errorf("issued %+v, want one grant carrying the tenant, session, grantee and reason asked for", f.issued)
	}

	long, _ := newGrants(f).RequestGrant(context.Background(), tenant, session, grantee, "r", 5*time.Minute)
	if !long.ExpiresAt.Equal(t0.Add(5 * time.Minute)) {
		t.Errorf("an explicit ttl expires %v, want %v", long.ExpiresAt, t0.Add(5*time.Minute))
	}
}

func TestRequestGrant_AStoreFailureIsReturned(t *testing.T) {
	f := &fakeGrantStore{issueErr: errors.New("disk full")}
	if _, err := newGrants(f).RequestGrant(context.Background(), tenant, session, grantee, "r", time.Hour); err == nil {
		t.Error("a grant that could not be stored (and so not audited) was reported as issued")
	}
}

func TestAuthorize_UsesTheNewestCurrentlyValidGrant(t *testing.T) {
	past, future := t0.Add(-time.Minute), t0.Add(time.Hour)
	revokedAt := t0.Add(-time.Second)
	expired, revoked, good := grant(past, nil), grant(future, &revokedAt), grant(future, nil)
	f := &fakeGrantStore{grants: []obs.Grant{expired, revoked, good}} // newest first

	got, err := newGrants(f).Authorize(context.Background(), tenant, session, grantee)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.GrantID != good.GrantID {
		t.Errorf("authorized by grant %s, want %s: an expired grant and a revoked one must both be skipped", got.GrantID, good.GrantID)
	}
	if len(f.refusals) != 0 {
		t.Errorf("%d refusals recorded for an authorized request", len(f.refusals))
	}
}

func TestAuthorize_WithNoValidGrantRefusesAndAuditsTheRefusal(t *testing.T) {
	revokedAt := t0.Add(-time.Second)
	cases := map[string][]obs.Grant{
		"no grants at all":    nil,
		"only expired":        {grant(t0.Add(-time.Minute), nil)},
		"only revoked":        {grant(t0.Add(time.Hour), &revokedAt)},
		"expires exactly now": {grant(t0, nil)}, // valid means strictly before ExpiresAt
	}
	for name, grants := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeGrantStore{grants: grants}
			_, err := newGrants(f).Authorize(context.Background(), tenant, session, grantee)
			if err == nil || !strings.Contains(err.Error(), "no valid content access grant") {
				t.Errorf("Authorize = %v, want a refusal", err)
			}
			if len(f.refusals) != 1 {
				t.Errorf("%d refusals recorded, want exactly 1: a denied attempt has its own audit trail", len(f.refusals))
			}
		})
	}
}

func TestAuthorize_StorageFailuresAreNotMistakenForARefusalOrAGrant(t *testing.T) {
	t.Run("listing fails", func(t *testing.T) {
		f := &fakeGrantStore{listErr: errors.New("boom")}
		if _, err := newGrants(f).Authorize(context.Background(), tenant, session, grantee); err == nil {
			t.Error("Authorize succeeded though the grants could not be read")
		}
		if len(f.refusals) != 0 {
			t.Error("a read failure was recorded as a refusal of the grantee")
		}
	})
	t.Run("the refusal cannot be audited", func(t *testing.T) {
		f := &fakeGrantStore{refusalErr: errors.New("boom")}
		_, err := newGrants(f).Authorize(context.Background(), tenant, session, grantee)
		if err == nil || strings.Contains(err.Error(), "no valid content access grant") {
			t.Errorf("Authorize = %v, want the audit failure itself", err)
		}
	})
}

func TestRead_ReturnsTheHistoryAndRecordsOneReceiptForTheWholeRead(t *testing.T) {
	history := []obs.EventDTO{{Seq: 1}, {Seq: 2}, {Seq: 3}}
	f := &fakeGrantStore{grants: []obs.Grant{grant(t0.Add(time.Hour), nil)}, history: history}

	got, err := newGrants(f).Read(context.Background(), tenant, session, grantee)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("Read returned %d events, want 3", len(got))
	}
	if len(f.accesses) != 1 || f.accesses[0] != 3 {
		t.Errorf("access receipts = %v, want exactly one, for 3 events (one per read, not per event)", f.accesses)
	}
}

func TestRead_WithoutAGrantOpensNothingAndRecordsNoAccess(t *testing.T) {
	f := &fakeGrantStore{history: []obs.EventDTO{{Seq: 1}}}
	got, err := newGrants(f).Read(context.Background(), tenant, session, grantee)
	if err == nil || got != nil {
		t.Fatalf("Read = (%v, %v), want a refusal with no content", got, err)
	}
	if f.opened != 0 {
		t.Error("the history was opened for a grantee with no valid grant")
	}
	if len(f.accesses) != 0 {
		t.Error("an access receipt was recorded for a read that never happened")
	}
}

func TestRead_WithholdsTheContentIfTheReceiptCannotBeRecorded(t *testing.T) {
	f := &fakeGrantStore{
		grants:    []obs.Grant{grant(t0.Add(time.Hour), nil)},
		history:   []obs.EventDTO{{Seq: 1}},
		accessErr: errors.New("chain unavailable"),
	}
	got, err := newGrants(f).Read(context.Background(), tenant, session, grantee)
	if err == nil {
		t.Fatal("Read succeeded though its receipt could not be recorded")
	}
	if got != nil {
		t.Errorf("Read returned %d events alongside the error: an unaudited read must return nothing", len(got))
	}
}

func TestRead_AHistoryFailureRecordsNoAccess(t *testing.T) {
	f := &fakeGrantStore{grants: []obs.Grant{grant(t0.Add(time.Hour), nil)}, historyErr: errors.New("decrypt failed")}
	if _, err := newGrants(f).Read(context.Background(), tenant, session, grantee); err == nil {
		t.Fatal("Read succeeded though the history could not be opened")
	}
	if len(f.accesses) != 0 {
		t.Error("an access receipt was recorded for a read that returned nothing")
	}
}
