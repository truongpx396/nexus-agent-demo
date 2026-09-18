package email

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeChannels struct {
	user, pass string
	ok         bool
	err        error
}

func (f fakeChannels) WebhookCredential(context.Context, uuid.UUID) (string, string, bool, error) {
	return f.user, f.pass, f.ok, f.err
}
func (f fakeChannels) SMTPConfig(context.Context, uuid.UUID) (SMTPConfig, error) {
	return SMTPConfig{Host: "smtp.example.com", Port: 587, FromAddress: "bot@example.com"}, nil
}

type fakeStarter struct {
	started bool
	calls   int
	req     RunRequest
}

func (f *fakeStarter) StartRun(_ context.Context, req RunRequest) (<-chan RunEvent, error) {
	f.started = true
	f.calls++
	f.req = req
	ch := make(chan RunEvent)
	close(ch)
	return ch, nil
}

// fakeResumer mirrors internal/surfaces/telegram's own (its doc comment).
type fakeResumer struct {
	resumed   bool
	sessionID uuid.UUID
	input     string
}

func (f *fakeResumer) ResumeConversation(_ context.Context, _ uuid.UUID, sessionID uuid.UUID, input string) (<-chan RunEvent, error) {
	f.resumed = true
	f.sessionID = sessionID
	f.input = input
	ch := make(chan RunEvent)
	close(ch)
	return ch, nil
}

// fakeLocker is surfaces.Locker's own test double — Acquire's every call
// result is queued up front (acquireResults), so a test can script
// contention (ok=false) as easily as success; Release just records what it
// was called with and, if set, closes done so a caller waiting on the
// async drainAndNotify goroutine (the only place Release is ever called
// from real production code) can synchronize on it instead of sleeping.
type fakeLocker struct {
	mu             sync.Mutex
	acquireResults []bool // consumed in order, one per Acquire call; exhausted results are all treated as false
	acquireCalls   []string
	releaseCalls   []releaseCall
	releaseErr     error
	done           chan struct{}
}

type releaseCall struct {
	sessionKey, token string
}

func (f *fakeLocker) Acquire(_ context.Context, sessionKey string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls = append(f.acquireCalls, sessionKey)
	ok := false
	if len(f.acquireResults) > 0 {
		ok = f.acquireResults[0]
		f.acquireResults = f.acquireResults[1:]
	}
	if !ok {
		return "", false, nil
	}
	return "token-" + sessionKey, true, nil
}

func (f *fakeLocker) Release(_ context.Context, sessionKey, token string) error {
	f.mu.Lock()
	f.releaseCalls = append(f.releaseCalls, releaseCall{sessionKey: sessionKey, token: token})
	f.mu.Unlock()
	if f.done != nil {
		close(f.done)
	}
	return f.releaseErr
}

func TestHandleWebhook_MissingBasicAuthRefusedBeforeBodyIsEverParsed(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{user: "bot", pass: "secret", ok: true}, Starter: starter}
	tenantID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/email/"+tenantID.String(), bytes.NewReader([]byte("not json at all")))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite missing Basic Auth")
	}
}

func TestHandleWebhook_WrongBasicAuthRefused(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{user: "bot", pass: "secret", ok: true}, Starter: starter}
	tenantID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/email/"+tenantID.String(), bytes.NewReader(nil))
	req.SetBasicAuth("bot", "wrong-password")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite a wrong password")
	}
}

func TestHandleWebhook_NoChannelConfiguredRefusesWithoutStartingARun(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{ok: false}, Starter: starter}
	tenantID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/email/"+tenantID.String(), bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no channel configured)", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite no configured channel")
	}
}

func TestHandleWebhook_RateLimitedRefusesBeforeBodyIsParsed(t *testing.T) {
	starter := &fakeStarter{}
	limiter := NewRateLimiter(1, time.Hour)
	tenantID := uuid.New()
	limiter.Allow(tenantID.String())

	s := &Server{Channels: fakeChannels{user: "bot", pass: "secret", ok: true}, Starter: starter, RateLimit: limiter}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/email/"+tenantID.String(), bytes.NewReader([]byte("would fail to parse if reached")))
	req.SetBasicAuth("bot", "secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (bucket pre-exhausted)", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite the rate limit")
	}
}

func TestDescriptor_ClaimsNoStreamingOrApprovalRendering(t *testing.T) {
	if Descriptor.SupportsStreaming {
		t.Error("Descriptor.SupportsStreaming = true, want false")
	}
	if Descriptor.CanRenderApprovalContext {
		t.Error("Descriptor.CanRenderApprovalContext = true, want false")
	}
}
