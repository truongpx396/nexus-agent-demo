package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

type fakeApprovals struct {
	list    []oversight.Approval
	get     oversight.Approval
	listErr error
	getErr  error
}

func (f fakeApprovals) Get(context.Context, uuid.UUID, uuid.UUID) (oversight.Approval, error) {
	return f.get, f.getErr
}
func (f fakeApprovals) ListPending(context.Context, uuid.UUID) ([]oversight.Approval, error) {
	return f.list, f.listErr
}

// fakeDecisions records which decision was taken, by whom and with what.
type fakeDecisions struct {
	called     string
	approvalID uuid.UUID
	decidedBy  string
	modified   json.RawMessage
	reason     string
	outcome    oversight.Outcome
	err        error
}

func (f *fakeDecisions) Grant(_ context.Context, _, approvalID uuid.UUID, decidedBy string) (oversight.Outcome, error) {
	f.called, f.approvalID, f.decidedBy = "grant", approvalID, decidedBy
	return f.outcome, f.err
}
func (f *fakeDecisions) GrantModified(_ context.Context, _, approvalID uuid.UUID, decidedBy string, modified json.RawMessage) (oversight.Outcome, error) {
	f.called, f.approvalID, f.decidedBy, f.modified = "grant_modified", approvalID, decidedBy, modified
	return f.outcome, f.err
}
func (f *fakeDecisions) Deny(_ context.Context, _, approvalID uuid.UUID, decidedBy, reason string) (oversight.Outcome, error) {
	f.called, f.approvalID, f.decidedBy, f.reason = "deny", approvalID, decidedBy, reason
	return f.outcome, f.err
}

func serve(userID uuid.UUID, a ApprovalSource, d DecisionSource) http.Handler {
	return restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: userID}, Handler{Approvals: a, Decisions: d}).Handler()
}

func TestListApprovals_RendersEachPendingApprovalAndAnEmptyListAsAnArray(t *testing.T) {
	ap := oversight.Approval{
		ApprovalID: uuid.New(), SessionID: uuid.New(), ToolID: "platform/email_send@v1", AskKind: "tool_call", Status: oversight.ApprovalPending,
		Context: oversight.ContextPackage{ToolID: "platform/email_send@v1", EffectClass: "external", Input: json.RawMessage(`{"to":"a@b.c"}`)},
	}
	rec := resttest.Do(t, serve(uuid.New(), fakeApprovals{list: []oversight.Approval{ap}}, &fakeDecisions{}), http.MethodGet, "/v1/approvals", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var views []struct {
		ApprovalID string          `json:"approval_id"`
		ToolID     string          `json:"tool_id"`
		Status     string          `json:"status"`
		Context    json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil || len(views) != 1 {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if views[0].ApprovalID != ap.ApprovalID.String() || views[0].Status != "pending" || views[0].ToolID != ap.ToolID {
		t.Errorf("view = %+v, want the approval's own id, tool and status", views[0])
	}
	if string(views[0].Context) == "" || !json.Valid(views[0].Context) {
		t.Errorf("context %q is not the decision-ready JSON an approver needs", views[0].Context)
	}

	empty := resttest.Do(t, serve(uuid.New(), fakeApprovals{}, &fakeDecisions{}), http.MethodGet, "/v1/approvals", "")
	if got := strings.TrimSpace(empty.Body.String()); got != "[]" {
		t.Errorf("an empty list rendered %q, want [] (clients iterate it)", got)
	}

	failing := resttest.Do(t, serve(uuid.New(), fakeApprovals{listErr: errors.New("db down")}, &fakeDecisions{}), http.MethodGet, "/v1/approvals", "")
	if failing.Code != http.StatusInternalServerError {
		t.Errorf("a failed read = %d, want 500", failing.Code)
	}
}

func TestGetApproval_DistinguishesABadIDFromAMissingApproval(t *testing.T) {
	id := uuid.New()
	h := serve(uuid.New(), fakeApprovals{get: oversight.Approval{ApprovalID: id, Status: oversight.ApprovalPending}}, &fakeDecisions{})
	if rec := resttest.Do(t, h, http.MethodGet, "/v1/approvals/"+id.String(), ""); rec.Code != http.StatusOK {
		t.Errorf("a known approval = %d, want 200", rec.Code)
	}
	if rec := resttest.Do(t, h, http.MethodGet, "/v1/approvals/not-a-uuid", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed id = %d, want 400", rec.Code)
	}
	missing := serve(uuid.New(), fakeApprovals{getErr: oversight.ErrNotFound}, &fakeDecisions{})
	if rec := resttest.Do(t, missing, http.MethodGet, "/v1/approvals/"+id.String(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown approval = %d, want 404", rec.Code)
	}
	down := serve(uuid.New(), fakeApprovals{getErr: errors.New("dial tcp 10.0.0.7:5432: refused")}, &fakeDecisions{})
	if rec := resttest.Do(t, down, http.MethodGet, "/v1/approvals/"+id.String(), ""); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("a failed lookup = %d %q, want a 500 that omits the cause (it used to answer 404 with it)", rec.Code, rec.Body)
	}
}

func TestGrantApproval_DecidesAsTheCallerAndOnlyModifiesWhenGivenAnInput(t *testing.T) {
	user, id, session := uuid.New(), uuid.New(), uuid.New()

	plain := &fakeDecisions{outcome: oversight.Outcome{ApprovalID: id, SessionID: session, Resuming: true}}
	rec := resttest.Do(t, serve(user, fakeApprovals{}, plain), http.MethodPost, "/v1/approvals/"+id.String()+"/grant", "")
	if rec.Code != http.StatusAccepted || plain.called != "grant" || plain.approvalID != id {
		t.Fatalf("plain grant: status %d, decision %q for %s", rec.Code, plain.called, plain.approvalID)
	}
	if plain.decidedBy != user.String() {
		t.Errorf("decidedBy = %q, want the authenticated user %q: a decision is attributed to whoever the token says, never to the request body", plain.decidedBy, user)
	}
	var out DecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SessionID != session || out.ApprovalID != id || out.Status != DecisionResponseStatusResuming {
		t.Errorf("response %q = %+v, want the approval and session named and the run resuming", rec.Body, out)
	}

	modified := &fakeDecisions{}
	resttest.Do(t, serve(user, fakeApprovals{}, modified), http.MethodPost, "/v1/approvals/"+id.String()+"/grant", `{"modified_input":{"to":"safe@b.c"}}`)
	if modified.called != "grant_modified" || string(modified.modified) != `{"to":"safe@b.c"}` {
		t.Errorf("modified grant: decision %q with input %q, want the approver's substitution passed through", modified.called, modified.modified)
	}

	bad := &fakeDecisions{}
	if rec := resttest.Do(t, serve(user, fakeApprovals{}, bad), http.MethodPost, "/v1/approvals/"+id.String()+"/grant", `{not json`); rec.Code != http.StatusBadRequest || bad.called != "" {
		t.Errorf("a malformed body = %d with decision %q, want 400 and no decision taken", rec.Code, bad.called)
	}
}

func TestDenyApproval_CarriesTheReason(t *testing.T) {
	user, id := uuid.New(), uuid.New()
	d := &fakeDecisions{}
	rec := resttest.Do(t, serve(user, fakeApprovals{}, d), http.MethodPost, "/v1/approvals/"+id.String()+"/deny", `{"reason":"wrong recipient"}`)
	if rec.Code != http.StatusAccepted || d.called != "deny" || d.reason != "wrong recipient" || d.decidedBy != user.String() {
		t.Errorf("deny: status %d, decision %q reason %q by %q", rec.Code, d.called, d.reason, d.decidedBy)
	}
}

// A decision whose hand-off to the queue failed is still a recorded decision, so
// it is answered 202, and the status says the run is not yet resuming.
func TestDecision_ADecisionWhoseHandOffFailedIsStillAccepted(t *testing.T) {
	id := uuid.New()
	d := &fakeDecisions{outcome: oversight.Outcome{ApprovalID: id, SessionID: uuid.New(), Resuming: false}}
	rec := resttest.Do(t, serve(uuid.New(), fakeApprovals{}, d), http.MethodPost, "/v1/approvals/"+id.String()+"/grant", "")
	var out DecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusAccepted || out.Status != DecisionResponseStatusDecided {
		t.Fatalf("status %d body %q, want 202 and status %q", rec.Code, rec.Body, DecisionResponseStatusDecided)
	}
}

func TestDecision_TellsAMissingApprovalFromOneAlreadyDecidedFromAFailure(t *testing.T) {
	id := uuid.New()
	post := func(err error) (int, string) {
		d := &fakeDecisions{err: err}
		rec := resttest.Do(t, serve(uuid.New(), fakeApprovals{}, d), http.MethodPost, "/v1/approvals/"+id.String()+"/grant", "")
		return rec.Code, rec.Body.String()
	}
	if code, _ := post(oversight.ErrNotFound); code != http.StatusNotFound {
		t.Errorf("a missing approval = %d, want 404", code)
	}
	if code, body := post(oversight.ApprovalNotPendingError{ApprovalID: id, Status: oversight.ApprovalGranted}); code != http.StatusConflict || !strings.Contains(body, "granted") {
		t.Errorf("an approval already decided = %d %q, want 409 saying what it is", code, body)
	}
	code, body := post(errors.New("dial tcp 10.0.0.7:5432: connection refused"))
	if code != http.StatusInternalServerError || strings.Contains(body, "10.0.0.7") {
		t.Errorf("an infrastructure failure = %d %q, want an opaque 500", code, body)
	}
}
