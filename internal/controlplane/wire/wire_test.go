package wire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/controlplane/wire"
)

var (
	idA  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	idB  = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	idC  = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	idD  = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	when = time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)

	// Every field of every V1 type is set, so a field that does not survive the
	// wire changes the value and fails the round trip; TestFixturesSetEveryField
	// keeps it that way when a field is added.
	admitReq = controlplane.AdmitRunV1{
		TenantID: idA, UserID: idB, SessionID: idC, SessionKey: "chat-42", SurfaceID: "telegram", DataLabel: "internal",
		RouteModelID: "model-x", RouteReason: map[string]string{"rule": "difficulty"}, Autonomy: "supervised",
		BudgetUSD: "0.05", HarnessDigest: []byte{0xde, 0xad, 0xbe, 0xef}, Conversational: true,
	}
	admitRes   = controlplane.AdmitRunResultV1{Admitted: true, Reason: "ok"}
	reserveReq = controlplane.ReserveBudgetV1{TenantID: idA, SessionID: idC, ModelID: "model-x", Purpose: "loop"}
	reserveRes = controlplane.ReserveBudgetResultV1{ReservationID: idD, Decision: "allow", Reason: "within ceiling", ReservedUSD: "0.01"}
	reportReq  = controlplane.ReportCostV1{ReservationID: idD, InputUncached: 10, InputCacheRead: 20, InputCacheWrite: 30, OutputTokens: 40, Reported: true}
	auditReq   = controlplane.EmitAuditReceiptV1{TenantID: idA, SessionID: idC, Seq: 7, EventID: idD, EventType: "tool_use", PayloadDigest: []byte{1, 2, 3}}
	approveReq = controlplane.RequestApprovalV1{
		TenantID: idA, SessionID: idC, ToolUseEventID: idD, ToolID: "platform/shell@v1", AskKind: "tool", CanonicalDigest: []byte{9, 8, 7},
		EffectClass: "mutating", Input: json.RawMessage(`{"cmd":"ls"}`),
	}
	approveRes = controlplane.RequestApprovalResultV1{ApprovalID: idB, ExpiresAt: when}
	contentReq = controlplane.AuthorizeContentAccessV1{TenantID: idA, SessionID: idC, GranteeID: idB, Reason: "audit", TTL: 15 * time.Minute}
	contentRes = controlplane.GrantV1{GrantID: idD, ExpiresAt: when}
)

// recordingPort answers every operation with a canned, fully populated result
// (or err) and remembers the request it was handed.
type recordingPort struct {
	admit    controlplane.AdmitRunV1
	reserve  controlplane.ReserveBudgetV1
	report   controlplane.ReportCostV1
	audit    controlplane.EmitAuditReceiptV1
	approval controlplane.RequestApprovalV1
	content  controlplane.AuthorizeContentAccessV1
	err      error
}

func (p *recordingPort) AdmitRun(_ context.Context, r controlplane.AdmitRunV1) (controlplane.AdmitRunResultV1, error) {
	p.admit = r
	return admitRes, p.err
}

func (p *recordingPort) ReserveBudget(_ context.Context, r controlplane.ReserveBudgetV1) (controlplane.ReserveBudgetResultV1, error) {
	p.reserve = r
	return reserveRes, p.err
}

func (p *recordingPort) ReportCost(_ context.Context, r controlplane.ReportCostV1) error {
	p.report = r
	return p.err
}

func (p *recordingPort) EmitAuditReceipt(_ context.Context, r controlplane.EmitAuditReceiptV1) error {
	p.audit = r
	return p.err
}

func (p *recordingPort) RequestApproval(_ context.Context, r controlplane.RequestApprovalV1) (controlplane.RequestApprovalResultV1, error) {
	p.approval = r
	return approveRes, p.err
}

func (p *recordingPort) AuthorizeContentAccess(_ context.Context, r controlplane.AuthorizeContentAccessV1) (controlplane.GrantV1, error) {
	p.content = r
	return contentRes, p.err
}

func serveWith(t *testing.T, p controlplane.Port) *wire.Client {
	t.Helper()
	srv := httptest.NewServer(wire.NewHandler(p))
	t.Cleanup(srv.Close)
	return &wire.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func expectEqual[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s did not survive the wire:\n got  %+v\n want %+v", what, got, want)
	}
}

func TestRoundTrip_EveryOperationCarriesEveryField(t *testing.T) {
	rec := &recordingPort{}
	c := serveWith(t, rec)
	ctx := context.Background()

	if res, err := c.AdmitRun(ctx, admitReq); err != nil {
		t.Fatalf("AdmitRun: %v", err)
	} else {
		expectEqual(t, "AdmitRunV1 request", rec.admit, admitReq)
		expectEqual(t, "AdmitRunResultV1", res, admitRes)
	}
	if res, err := c.ReserveBudget(ctx, reserveReq); err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	} else {
		expectEqual(t, "ReserveBudgetV1 request", rec.reserve, reserveReq)
		expectEqual(t, "ReserveBudgetResultV1", res, reserveRes)
	}
	if err := c.ReportCost(ctx, reportReq); err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	expectEqual(t, "ReportCostV1 request", rec.report, reportReq)
	if err := c.EmitAuditReceipt(ctx, auditReq); err != nil {
		t.Fatalf("EmitAuditReceipt: %v", err)
	}
	expectEqual(t, "EmitAuditReceiptV1 request", rec.audit, auditReq)
	if res, err := c.RequestApproval(ctx, approveReq); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	} else {
		expectEqual(t, "RequestApprovalV1 request", rec.approval, approveReq)
		expectEqual(t, "RequestApprovalResultV1", res, approveRes)
	}
	if res, err := c.AuthorizeContentAccess(ctx, contentReq); err != nil {
		t.Fatalf("AuthorizeContentAccess: %v", err)
	} else {
		expectEqual(t, "AuthorizeContentAccessV1 request", rec.content, contentReq)
		expectEqual(t, "GrantV1", res, contentRes)
	}
}

// A round trip through the same Go types cannot catch a wrong field name (both
// ends agree on the mistake), so the names a non-Go client would depend on are
// pinned here. Changing one is a breaking change to v1.
func TestWireNames_Golden(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"AdmitRunV1", admitReq, `{"tenant_id":"11111111-1111-1111-1111-111111111111","user_id":"22222222-2222-2222-2222-222222222222","session_id":"33333333-3333-3333-3333-333333333333","session_key":"chat-42","surface_id":"telegram","data_label":"internal","route_model_id":"model-x","route_reason":{"rule":"difficulty"},"autonomy":"supervised","budget_usd":"0.05","harness_digest":"3q2+7w==","conversational":true}`},
		{"AdmitRunResultV1", admitRes, `{"admitted":true,"reason":"ok"}`},
		{"ReserveBudgetV1", reserveReq, `{"tenant_id":"11111111-1111-1111-1111-111111111111","session_id":"33333333-3333-3333-3333-333333333333","model_id":"model-x","purpose":"loop"}`},
		{"ReserveBudgetResultV1", reserveRes, `{"reservation_id":"44444444-4444-4444-4444-444444444444","decision":"allow","reason":"within ceiling","reserved_usd":"0.01"}`},
		{"ReportCostV1", reportReq, `{"reservation_id":"44444444-4444-4444-4444-444444444444","input_uncached":10,"input_cache_read":20,"input_cache_write":30,"output_tokens":40,"reported":true}`},
		{"EmitAuditReceiptV1", auditReq, `{"tenant_id":"11111111-1111-1111-1111-111111111111","session_id":"33333333-3333-3333-3333-333333333333","seq":7,"event_id":"44444444-4444-4444-4444-444444444444","event_type":"tool_use","payload_digest":"AQID"}`},
		{"RequestApprovalV1", approveReq, `{"tenant_id":"11111111-1111-1111-1111-111111111111","session_id":"33333333-3333-3333-3333-333333333333","tool_use_event_id":"44444444-4444-4444-4444-444444444444","tool_id":"platform/shell@v1","ask_kind":"tool","canonical_digest":"CQgH","effect_class":"mutating","input":{"cmd":"ls"}}`},
		{"RequestApprovalResultV1", approveRes, `{"approval_id":"22222222-2222-2222-2222-222222222222","expires_at":"2026-11-02T09:30:00Z"}`},
		{"AuthorizeContentAccessV1", contentReq, `{"tenant_id":"11111111-1111-1111-1111-111111111111","session_id":"33333333-3333-3333-3333-333333333333","grantee_id":"22222222-2222-2222-2222-222222222222","reason":"audit","ttl_ns":900000000000}`},
		{"GrantV1", contentRes, `{"grant_id":"44444444-4444-4444-4444-444444444444","expires_at":"2026-11-02T09:30:00Z"}`},
	}
	for _, tc := range cases {
		got, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s wire form changed:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

// If a field is added to a V1 type without being set in the fixtures above, the
// round trip would pass vacuously for it; fail instead.
func TestFixturesSetEveryField(t *testing.T) {
	for _, v := range []any{admitReq, admitRes, reserveReq, reserveRes, reportReq, auditReq, approveReq, approveRes, contentReq, contentRes} {
		rv := reflect.ValueOf(v)
		for i := 0; i < rv.NumField(); i++ {
			if rv.Field(i).IsZero() {
				t.Errorf("%s.%s is zero in the test fixtures: set it so the round trip covers it", rv.Type().Name(), rv.Type().Field(i).Name)
			}
		}
	}
}

func TestFailureCrossesTheWireAsAnError(t *testing.T) {
	c := serveWith(t, &recordingPort{err: errors.New("ledger down")})
	ctx := context.Background()
	calls := map[string]func() error{
		"AdmitRun":               func() error { _, err := c.AdmitRun(ctx, admitReq); return err },
		"ReserveBudget":          func() error { _, err := c.ReserveBudget(ctx, reserveReq); return err },
		"ReportCost":             func() error { return c.ReportCost(ctx, reportReq) },
		"EmitAuditReceipt":       func() error { return c.EmitAuditReceipt(ctx, auditReq) },
		"RequestApproval":        func() error { _, err := c.RequestApproval(ctx, approveReq); return err },
		"AuthorizeContentAccess": func() error { _, err := c.AuthorizeContentAccess(ctx, contentReq); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "ledger down") || !strings.Contains(err.Error(), "status 500") {
			t.Errorf("%s: err = %v, want one carrying the backend's message and the status", name, err)
		}
	}
}

// A refusal is data, not a failure: LocalPort answers an invalid autonomy with
// Admitted false and no error, and that distinction must survive the transport.
func TestRefusalIsDataNotAnError(t *testing.T) {
	c := serveWith(t, controlplane.NewLocalPort(nil, nil, nil, nil, nil)) // an invalid autonomy never reaches a backend
	bad := admitReq
	bad.Autonomy = "reckless"
	res, err := c.AdmitRun(context.Background(), bad)
	if err != nil {
		t.Fatalf("a refusal came back as an error: %v", err)
	}
	if res.Admitted || !strings.Contains(res.Reason, "reckless") {
		t.Errorf("result = %+v, want a refusal that names the bad autonomy", res)
	}
}

func TestHandler_RejectsAMalformedBodyAndAWrongMethod(t *testing.T) {
	srv := httptest.NewServer(wire.NewHandler(&recordingPort{}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/controlplane/v1/admit-run"

	resp, err := srv.Client().Post(url, "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", resp.StatusCode)
	}

	resp, err = srv.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", resp.StatusCode)
	}
}
