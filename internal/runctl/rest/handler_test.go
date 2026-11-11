package rest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// fakeController records the last operation and answers with a fixed error.
type fakeController struct {
	op        string
	sessionID uuid.UUID
	arg       string
	atSeq     int64
	overrides runctl.ForkOverrides
	err       error
	fork      runctl.ForkResult
}

func (f *fakeController) Cancel(_ context.Context, _, id uuid.UUID, reason string) error {
	f.op, f.sessionID, f.arg = "cancel", id, reason
	return f.err
}
func (f *fakeController) Send(_ context.Context, _, id uuid.UUID, input string) error {
	f.op, f.sessionID, f.arg = "send", id, input
	return f.err
}
func (f *fakeController) TightenAutonomy(_ context.Context, _, id uuid.UUID, target string) error {
	f.op, f.sessionID, f.arg = "tighten", id, target
	return f.err
}
func (f *fakeController) Fork(_ context.Context, _, id uuid.UUID, atSeq int64, o runctl.ForkOverrides) (runctl.ForkResult, error) {
	f.op, f.sessionID, f.atSeq, f.overrides = "fork", id, atSeq, o
	return f.fork, f.err
}

func serve(c Controller) http.Handler {
	return restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: uuid.New()}, Handler{Control: c}).Handler()
}

func TestCancelRun_PassesTheReasonAndMapsAFailureTo500(t *testing.T) {
	id := uuid.New()
	f := &fakeController{}
	rec := resttest.Do(t, serve(f), http.MethodPost, "/v1/runs/"+id.String()+"/cancel", `{"reason":"user pressed stop"}`)
	if rec.Code != http.StatusOK || f.op != "cancel" || f.sessionID != id || f.arg != "user pressed stop" {
		t.Fatalf("cancel: status %d, %s of %s with %q", rec.Code, f.op, f.sessionID, f.arg)
	}
	if rec := resttest.Do(t, serve(&fakeController{err: errors.New("boom")}), http.MethodPost, "/v1/runs/"+id.String()+"/cancel", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed cancel = %d, want 500", rec.Code)
	}
	if rec := resttest.Do(t, serve(f), http.MethodPost, "/v1/runs/nope/cancel", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed run id = %d, want 400", rec.Code)
	}
}

// Steering is one call to Send: whether the session is mid-run or a paused
// conversation is runctl's to tell, so the handler never reads the session.
func TestSteerRun_DeliversTheMessageThroughSend(t *testing.T) {
	id := uuid.New()
	f := &fakeController{}
	rec := resttest.Do(t, serve(f), http.MethodPost, "/v1/runs/"+id.String()+"/steer", `{"input":"also check the logs"}`)
	if rec.Code != http.StatusOK || f.op != "send" || f.arg != "also check the logs" {
		t.Fatalf("steer: status %d, %s with %q", rec.Code, f.op, f.arg)
	}
	if rec := resttest.Do(t, serve(f), http.MethodPost, "/v1/runs/"+id.String()+"/steer", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed body = %d, want 400", rec.Code)
	}
	if rec := resttest.Do(t, serve(&fakeController{err: errors.New("terminal")}), http.MethodPost, "/v1/runs/"+id.String()+"/steer", `{"input":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("a refused steer = %d, want 500", rec.Code)
	}
}

// A refused widening of autonomy is the caller's mistake, not a server fault.
func TestTightenAutonomy_MapsARefusalTo400(t *testing.T) {
	id := uuid.New()
	rec := resttest.Do(t, serve(&fakeController{}), http.MethodPost, "/v1/runs/"+id.String()+"/autonomy", `{"target":"read_only"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tighten = %d (%s)", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got["autonomy_level"] != "read_only" {
		t.Errorf("body %q, want the level that was applied", rec.Body)
	}
	if rec := resttest.Do(t, serve(&fakeController{err: fmt.Errorf("%w: cannot widen", runctl.ErrInvalidArgument)}), http.MethodPost, "/v1/runs/"+id.String()+"/autonomy", `{"target":"autonomous"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a refused widening = %d, want 400", rec.Code)
	}
}

func TestForkRun_ReportsDigestDivergenceAndHexEncodesTheDigests(t *testing.T) {
	id, child := uuid.New(), uuid.New()
	f := &fakeController{fork: runctl.ForkResult{SessionID: child, ParentDigest: []byte{0xab, 0xcd}, ChildDigest: []byte{0x01, 0x02}, DigestDiverged: true}}
	rec := resttest.Do(t, serve(f), http.MethodPost, "/v1/runs/"+id.String()+"/fork", `{"at_seq":7,"model":"claude-haiku-5-5"}`)
	if rec.Code != http.StatusOK || f.atSeq != 7 || f.overrides.ModelID != "claude-haiku-5-5" {
		t.Fatalf("fork: status %d at_seq %d overrides %+v", rec.Code, f.atSeq, f.overrides)
	}
	var view ForkRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	if view.SessionID != child || !view.DigestDiverged || view.ParentDigestHex != hex.EncodeToString([]byte{0xab, 0xcd}) || view.ChildDigestHex != "0102" {
		t.Errorf("view = %+v, want the child session, the divergence flag and hex digests: a fork must never read as a byte-identical replay", view)
	}
}

// Every operation tells the caller what the caller can act on (404, 409, 400)
// and nothing about an infrastructure failure, which is a logged 500.
func TestRunControl_MapsRefusalsToStatusesAndNeverEchoesInfrastructureErrors(t *testing.T) {
	id := uuid.New().String()
	ops := map[string]struct{ path, body string }{
		"cancel":   {"/v1/runs/" + id + "/cancel", ""},
		"steer":    {"/v1/runs/" + id + "/steer", `{"input":"x"}`},
		"autonomy": {"/v1/runs/" + id + "/autonomy", `{"target":"read_only"}`},
		"fork":     {"/v1/runs/" + id + "/fork", `{"at_seq":1}`},
	}
	outcomes := []struct {
		name string
		err  error
		want int
	}{
		{"a missing session", fmt.Errorf("fork: %w", runctl.ErrSessionNotFound), http.StatusNotFound},
		{"a session in the wrong state", fmt.Errorf("%w: already terminal", runctl.ErrSessionState), http.StatusConflict},
		{"an argument it cannot act on", fmt.Errorf("%w: no events", runctl.ErrInvalidArgument), http.StatusBadRequest},
		{"an infrastructure failure", errors.New("dial tcp 10.0.0.7:5432: connection refused"), http.StatusInternalServerError},
	}
	for op, req := range ops {
		for _, o := range outcomes {
			t.Run(op+"/"+o.name, func(t *testing.T) {
				rec := resttest.Do(t, serve(&fakeController{err: o.err}), http.MethodPost, req.path, req.body)
				if rec.Code != o.want {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, o.want, rec.Body)
				}
				if o.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "10.0.0.7") {
					t.Errorf("a 500 echoed the infrastructure error: %q", rec.Body)
				}
			})
		}
	}
}
