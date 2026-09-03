package email

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
	req     RunRequest
}

func (f *fakeStarter) StartRun(_ context.Context, req RunRequest) (<-chan RunEvent, error) {
	f.started = true
	f.req = req
	ch := make(chan RunEvent)
	close(ch)
	return ch, nil
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
