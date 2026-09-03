package telegram

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
	secret string
	ok     bool
	err    error
}

func (f fakeChannels) WebhookSecret(context.Context, uuid.UUID) (string, bool, error) {
	return f.secret, f.ok, f.err
}
func (f fakeChannels) BotToken(context.Context, uuid.UUID) (string, error) { return "bot-token", nil }

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

func TestHandleWebhook_WrongSecretRefusedBeforeBodyIsEverParsed(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{secret: "correct-secret", ok: true}, Starter: starter}
	tenantID := uuid.New()

	// A body that would fail JSON parsing if it were ever reached — proves
	// the auth check runs BEFORE any parsing is attempted
	// (docs/constitution.md: "before the kernel sees the payload").
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/telegram/"+tenantID.String(), bytes.NewReader([]byte("not json at all")))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite a wrong secret token")
	}
}

func TestHandleWebhook_NoChannelConfiguredRefusesWithoutStartingARun(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{ok: false}, Starter: starter}
	tenantID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/telegram/"+tenantID.String(), bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no channel configured)", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite no configured channel")
	}
}

// TestHandleWebhook_RateLimitedRefusesBeforeBodyIsParsed pre-exhausts the
// bucket directly (RateLimiter is unit-tested on its own merits in
// ratelimit_test.go) so the ONE HTTP request this test issues is the
// rate-limited one — proving the 429 short-circuit happens before
// startRun's DB-touching session creation is ever reached, without this
// unit test needing a real Store (internal/store.Store.InTenantTx panics
// on a nil pool; a request that gets past rate-limiting is covered by
// webhook_integration_test.go instead, the same unit/integration split
// internal/surfaces/rest's own tests draw).
func TestHandleWebhook_RateLimitedRefusesBeforeBodyIsParsed(t *testing.T) {
	starter := &fakeStarter{}
	limiter := NewRateLimiter(1, time.Hour)
	tenantID := uuid.New()
	limiter.Allow(tenantID.String()) // consume the one token this bucket starts with

	s := &Server{Channels: fakeChannels{secret: "s", ok: true}, Starter: starter, RateLimit: limiter}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/telegram/"+tenantID.String(), bytes.NewReader([]byte("would fail to parse if reached")))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s")
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
		t.Error("Descriptor.SupportsStreaming = true, want false — this surface has no server-push channel of its own")
	}
	if Descriptor.CanRenderApprovalContext {
		t.Error("Descriptor.CanRenderApprovalContext = true, want false — a chat bot gets the one-line fallback, not structured rendering")
	}
}
