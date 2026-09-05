package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeSessionStore/fakeBudgetBackend/fakeAuditBackend/fakeApprovalBackend/
// fakeContentAccessBackend are minimal in-memory fakes for LocalPort's four
// backend seams (README task 13.15, closing production-readiness finding
// F15) — LocalPort itself touches no database, so its own orchestration
// logic (the AdmitRun autonomy check, and otherwise pure delegation) is
// fully testable without Postgres; the two REST integration tests
// (tests/integration/rest_run_test.go, phase7_harness_growth_test.go)
// already prove AdmitRun end to end against a real one.
type fakeSessionStore struct {
	called bool
	gotReq AdmitRunV1
	result AdmitRunResultV1
	err    error
}

func (f *fakeSessionStore) CreateSession(_ context.Context, req AdmitRunV1) (AdmitRunResultV1, error) {
	f.called = true
	f.gotReq = req
	return f.result, f.err
}

type fakeBudgetBackend struct {
	reserveReq ReserveBudgetV1
	reserveRes ReserveBudgetResultV1
	reserveErr error
	reportReq  ReportCostV1
	reportErr  error
}

func (f *fakeBudgetBackend) Reserve(_ context.Context, req ReserveBudgetV1) (ReserveBudgetResultV1, error) {
	f.reserveReq = req
	return f.reserveRes, f.reserveErr
}

func (f *fakeBudgetBackend) ReportCost(_ context.Context, req ReportCostV1) error {
	f.reportReq = req
	return f.reportErr
}

type fakeAuditBackend struct {
	gotReq EmitAuditReceiptV1
	err    error
}

func (f *fakeAuditBackend) EmitReceipt(_ context.Context, req EmitAuditReceiptV1) error {
	f.gotReq = req
	return f.err
}

type fakeApprovalBackend struct {
	gotReq RequestApprovalV1
	result RequestApprovalResultV1
	err    error
}

func (f *fakeApprovalBackend) RequestApproval(_ context.Context, req RequestApprovalV1) (RequestApprovalResultV1, error) {
	f.gotReq = req
	return f.result, f.err
}

type fakeContentAccessBackend struct {
	gotReq AuthorizeContentAccessV1
	result GrantV1
	err    error
}

func (f *fakeContentAccessBackend) AuthorizeContentAccess(_ context.Context, req AuthorizeContentAccessV1) (GrantV1, error) {
	f.gotReq = req
	return f.result, f.err
}

func TestLocalPort_AdmitRun_RejectsInvalidAutonomyWithoutCallingBackend(t *testing.T) {
	sessions := &fakeSessionStore{}
	port := NewLocalPort(sessions, &fakeBudgetBackend{}, &fakeAuditBackend{}, &fakeApprovalBackend{}, &fakeContentAccessBackend{})

	result, err := port.AdmitRun(context.Background(), AdmitRunV1{Autonomy: "godmode"})
	if err != nil {
		t.Fatalf("AdmitRun returned an error for a normal refusal: %v", err)
	}
	if result.Admitted {
		t.Fatal("AdmitRun admitted an invalid autonomy value")
	}
	if result.Reason == "" {
		t.Error("AdmitRun's refusal carries no reason")
	}
	if sessions.called {
		t.Error("SessionStore.CreateSession was called despite an invalid autonomy value")
	}
}

func TestLocalPort_AdmitRun_DelegatesValidRequestToSessionStore(t *testing.T) {
	sessionID := uuid.New()
	sessions := &fakeSessionStore{result: AdmitRunResultV1{Admitted: true}}
	port := NewLocalPort(sessions, &fakeBudgetBackend{}, &fakeAuditBackend{}, &fakeApprovalBackend{}, &fakeContentAccessBackend{})

	req := AdmitRunV1{SessionID: sessionID, Autonomy: "supervised"}
	result, err := port.AdmitRun(context.Background(), req)
	if err != nil {
		t.Fatalf("AdmitRun: %v", err)
	}
	if !result.Admitted {
		t.Fatal("AdmitRun did not admit a valid request the backend approved")
	}
	if sessions.gotReq.SessionID != sessionID {
		t.Errorf("SessionStore.CreateSession got SessionID %s, want %s", sessions.gotReq.SessionID, sessionID)
	}
}

func TestLocalPort_AdmitRun_PropagatesBackendError(t *testing.T) {
	wantErr := errors.New("db unavailable")
	sessions := &fakeSessionStore{err: wantErr}
	port := NewLocalPort(sessions, &fakeBudgetBackend{}, &fakeAuditBackend{}, &fakeApprovalBackend{}, &fakeContentAccessBackend{})

	_, err := port.AdmitRun(context.Background(), AdmitRunV1{Autonomy: "read_only"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AdmitRun error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestLocalPort_ReserveBudget_Delegates(t *testing.T) {
	budget := &fakeBudgetBackend{reserveRes: ReserveBudgetResultV1{ReservationID: uuid.New(), Decision: "allow"}}
	port := NewLocalPort(&fakeSessionStore{}, budget, &fakeAuditBackend{}, &fakeApprovalBackend{}, &fakeContentAccessBackend{})

	req := ReserveBudgetV1{ModelID: "claude-sonnet-5", Purpose: "turn"}
	result, err := port.ReserveBudget(context.Background(), req)
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	if result.Decision != "allow" {
		t.Errorf("Decision = %q, want %q", result.Decision, "allow")
	}
	if budget.reserveReq.ModelID != "claude-sonnet-5" {
		t.Errorf("BudgetBackend.Reserve got ModelID %q, want %q", budget.reserveReq.ModelID, "claude-sonnet-5")
	}
}

func TestLocalPort_ReportCost_Delegates(t *testing.T) {
	budget := &fakeBudgetBackend{}
	port := NewLocalPort(&fakeSessionStore{}, budget, &fakeAuditBackend{}, &fakeApprovalBackend{}, &fakeContentAccessBackend{})

	reservationID := uuid.New()
	if err := port.ReportCost(context.Background(), ReportCostV1{ReservationID: reservationID, OutputTokens: 42, Reported: true}); err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	if budget.reportReq.ReservationID != reservationID {
		t.Errorf("BudgetBackend.ReportCost got ReservationID %s, want %s", budget.reportReq.ReservationID, reservationID)
	}
	if budget.reportReq.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, want 42", budget.reportReq.OutputTokens)
	}
}

func TestLocalPort_EmitAuditReceipt_Delegates(t *testing.T) {
	auditBackend := &fakeAuditBackend{}
	port := NewLocalPort(&fakeSessionStore{}, &fakeBudgetBackend{}, auditBackend, &fakeApprovalBackend{}, &fakeContentAccessBackend{})

	eventID := uuid.New()
	if err := port.EmitAuditReceipt(context.Background(), EmitAuditReceiptV1{EventID: eventID, EventType: "tool_use", Seq: 3}); err != nil {
		t.Fatalf("EmitAuditReceipt: %v", err)
	}
	if auditBackend.gotReq.EventID != eventID || auditBackend.gotReq.Seq != 3 {
		t.Errorf("AuditBackend.EmitReceipt got %+v", auditBackend.gotReq)
	}
}

func TestLocalPort_RequestApproval_Delegates(t *testing.T) {
	approvalID := uuid.New()
	approval := &fakeApprovalBackend{result: RequestApprovalResultV1{ApprovalID: approvalID, ExpiresAt: time.Now().Add(time.Hour)}}
	port := NewLocalPort(&fakeSessionStore{}, &fakeBudgetBackend{}, &fakeAuditBackend{}, approval, &fakeContentAccessBackend{})

	result, err := port.RequestApproval(context.Background(), RequestApprovalV1{ToolID: "platform/shell@1", AskKind: "once"})
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if result.ApprovalID != approvalID {
		t.Errorf("ApprovalID = %s, want %s", result.ApprovalID, approvalID)
	}
	if approval.gotReq.ToolID != "platform/shell@1" {
		t.Errorf("ApprovalBackend.RequestApproval got ToolID %q", approval.gotReq.ToolID)
	}
}

func TestLocalPort_AuthorizeContentAccess_Delegates(t *testing.T) {
	grantID := uuid.New()
	content := &fakeContentAccessBackend{result: GrantV1{GrantID: grantID}}
	port := NewLocalPort(&fakeSessionStore{}, &fakeBudgetBackend{}, &fakeAuditBackend{}, &fakeApprovalBackend{}, content)

	granteeID := uuid.New()
	result, err := port.AuthorizeContentAccess(context.Background(), AuthorizeContentAccessV1{GranteeID: granteeID, Reason: "incident review", TTL: time.Hour})
	if err != nil {
		t.Fatalf("AuthorizeContentAccess: %v", err)
	}
	if result.GrantID != grantID {
		t.Errorf("GrantID = %s, want %s", result.GrantID, grantID)
	}
	if content.gotReq.GranteeID != granteeID {
		t.Errorf("ContentAccessBackend.AuthorizeContentAccess got GranteeID %s, want %s", content.gotReq.GranteeID, granteeID)
	}
}
