package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
}

func (f *fakeDecisions) Grant(_ context.Context, _, approvalID uuid.UUID, decidedBy string) oversight.Outcome {
	f.called, f.approvalID, f.decidedBy = "grant", approvalID, decidedBy
	return f.outcome
}
func (f *fakeDecisions) GrantModified(_ context.Context, _, approvalID uuid.UUID, decidedBy string, modified json.RawMessage) oversight.Outcome {
	f.called, f.approvalID, f.decidedBy, f.modified = "grant_modified", approvalID, decidedBy, modified
	return f.outcome
}
func (f *fakeDecisions) Deny(_ context.Context, _, approvalID uuid.UUID, decidedBy, reason string) oversight.Outcome {
	f.called, f.approvalID, f.decidedBy, f.reason = "deny", approvalID, decidedBy, reason
	return f.outcome
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
	if got := empty.Body.String(); got != "[]\n" {
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
}

func TestGrantApproval_DecidesAsTheCallerAndOnlyModifiesWhenGivenAnInput(t *testing.T) {
	user, id := uuid.New(), uuid.New()

	plain := &fakeDecisions{outcome: oversight.Outcome{SessionID: "s1", EventsAppended: 4}}
	rec := resttest.Do(t, serve(user, fakeApprovals{}, plain), http.MethodPost, "/v1/approvals/"+id.String()+"/grant", "")
	if rec.Code != http.StatusOK || plain.called != "grant" || plain.approvalID != id {
		t.Fatalf("plain grant: status %d, decision %q for %s", rec.Code, plain.called, plain.approvalID)
	}
	if plain.decidedBy != user.String() {
		t.Errorf("decidedBy = %q, want the authenticated user %q: a decision is attributed to whoever the token says, never to the request body", plain.decidedBy, user)
	}
	var out ResumeOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SessionID != "s1" || out.EventsAppended != 4 {
		t.Errorf("response %q = %+v, want the decision's outcome", rec.Body, out)
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
	if rec.Code != http.StatusOK || d.called != "deny" || d.reason != "wrong recipient" || d.decidedBy != user.String() {
		t.Errorf("deny: status %d, decision %q reason %q by %q", rec.Code, d.called, d.reason, d.decidedBy)
	}
}
