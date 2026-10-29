package rest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

// fakeReader is a runs.Reader with no database behind it, so these tests pin
// the REST surface's own behavior — status codes, ownership, what is and is
// not shown — separately from what the Postgres adapter reads.
type fakeReader struct {
	run      runs.RunView
	getErr   error
	list     []runs.RunView
	listErr  error
	events   []eventlog.Event
	eventErr error
	opened   int
}

func (f *fakeReader) GetRun(context.Context, uuid.UUID, uuid.UUID) (runs.RunView, error) {
	return f.run, f.getErr
}
func (f *fakeReader) ListRuns(context.Context, uuid.UUID, uuid.UUID, int) ([]runs.RunView, error) {
	return f.list, f.listErr
}
func (f *fakeReader) Events(context.Context, uuid.UUID, uuid.UUID) ([]eventlog.Event, error) {
	return f.events, f.eventErr
}
func (f *fakeReader) EventOpener(uuid.UUID) runs.EventOpener { return f }
func (f *fakeReader) Open(_ context.Context, e eventlog.Event) ([]byte, error) {
	f.opened++
	return []byte(`{"body":"plain-` + string(e.Type) + `"}`), nil
}

func readsServer(t *testing.T, tenantID, userID uuid.UUID, r *fakeReader) http.Handler {
	t.Helper()
	s := NewServer(nil, r)
	s.Verifier = fakeVerifier{wantToken: "good", tenantID: tenantID, userID: userID}
	return s.Handler()
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGetRun_StatusCodesDistinguishMissingForbiddenAndBroken(t *testing.T) {
	tenantID, userID, runID := uuid.New(), uuid.New(), uuid.New()
	mine := runs.RunView{RunID: runID, UserID: userID, Status: "completed"}
	reason := "completed"
	mine.TerminalReason = &reason

	tests := []struct {
		name   string
		reader *fakeReader
		want   int
		body   string
	}{
		{"own run", &fakeReader{run: mine}, http.StatusOK, `"status":"completed"`},
		{"unknown run", &fakeReader{getErr: runs.ErrNotFound}, http.StatusNotFound, "run not found"},
		{"someone else's run", &fakeReader{run: runs.RunView{RunID: runID, UserID: uuid.New()}}, http.StatusForbidden, "forbidden"},
		// A failed read used to surface as 404, hiding an outage behind a client error.
		{"read failure is a server error, not a 404", &fakeReader{getErr: errors.New("connection refused")}, http.StatusInternalServerError, "connection refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(readsServer(t, tenantID, userID, tc.reader), "/v1/runs/"+runID.String())
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("got %d %q, want %d containing %q", rec.Code, rec.Body.String(), tc.want, tc.body)
			}
		})
	}
}

func TestListSessions_MapsRunsToSummaries(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	id := uuid.New()
	created := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	r := &fakeReader{list: []runs.RunView{{RunID: id, UserID: userID, Status: "running", CreatedAt: created, AutonomyLevel: "supervised"}}}

	rec := get(readsServer(t, tenantID, userID, r), "/v1/sessions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"run_id":"` + id.String() + `"`, `"status":"running"`, `"autonomy_level":"supervised"`, `2026-10-29T12:00:00Z`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %s lacks %s", rec.Body, want)
		}
	}

	rec = get(readsServer(t, tenantID, userID, &fakeReader{listErr: errors.New("boom")}), "/v1/sessions")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failing reader gave %d, want 500", rec.Code)
	}
}

func TestEvents_ReplaysDecryptedHistoryAndNeverShowsReasoning(t *testing.T) {
	tenantID, userID, runID := uuid.New(), uuid.New(), uuid.New()
	ev := func(seq int64, typ eventlog.EventType) eventlog.Event {
		return eventlog.Event{EventID: uuid.New(), SessionID: runID, TenantID: tenantID, Seq: seq, Type: typ, KeyID: "k", Actor: eventlog.ActorSystem, CreatedAt: time.Now()}
	}
	r := &fakeReader{
		run:    runs.RunView{RunID: runID, UserID: userID},
		events: []eventlog.Event{ev(1, eventlog.EventUserMessage), ev(2, eventlog.EventThought), ev(3, eventlog.EventContent), ev(4, eventlog.EventTerminal)},
	}

	rec := get(readsServer(t, tenantID, userID, r), "/v1/runs/"+runID.String()+"/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: user_message", `plain-user_message`, "event: content", `plain-content`, "event: terminal"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "plain-thought") {
		t.Error("reasoning was decrypted and shown: a thought must never carry a body, even to the run's own audience")
	}
	if r.opened != 3 {
		t.Errorf("opened %d events, want 3 (everything except the thought)", r.opened)
	}
}

func TestEvents_RefusesAnotherUsersRunBeforeStreaming(t *testing.T) {
	tenantID, userID, runID := uuid.New(), uuid.New(), uuid.New()
	r := &fakeReader{run: runs.RunView{RunID: runID, UserID: uuid.New()}, events: []eventlog.Event{{Type: eventlog.EventContent}}}

	rec := get(readsServer(t, tenantID, userID, r), "/v1/runs/"+runID.String()+"/events")
	if rec.Code != http.StatusForbidden || r.opened != 0 {
		t.Fatalf("got %d with %d event(s) opened; another user's run must be refused before any content is read", rec.Code, r.opened)
	}
}
