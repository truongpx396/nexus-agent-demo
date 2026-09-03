package zalo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func (f fakeChannels) AppSecret(context.Context, uuid.UUID) (string, bool, error) {
	return f.secret, f.ok, f.err
}
func (f fakeChannels) AccessToken(context.Context, uuid.UUID) (string, error) { return "oa-token", nil }

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

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHandleWebhook_WrongSignatureRefusedBeforeBodyIsEverParsed(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{secret: "correct-secret", ok: true}, Starter: starter}
	tenantID := uuid.New()

	body := []byte("not json at all")
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/zalo/"+tenantID.String(), bytes.NewReader(body))
	req.Header.Set("X-ZEvent-Signature", sign(body, "wrong-secret"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite a wrong signature")
	}
}

func TestHandleWebhook_NoChannelConfiguredRefusesWithoutStartingARun(t *testing.T) {
	starter := &fakeStarter{}
	s := &Server{Channels: fakeChannels{ok: false}, Starter: starter}
	tenantID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/zalo/"+tenantID.String(), bytes.NewReader(nil))
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

	s := &Server{Channels: fakeChannels{secret: "s", ok: true}, Starter: starter, RateLimit: limiter}
	body := []byte("would fail to parse if reached")
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/zalo/"+tenantID.String(), bytes.NewReader(body))
	req.Header.Set("X-ZEvent-Signature", sign(body, "s"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (bucket pre-exhausted)", rec.Code)
	}
	if starter.started {
		t.Fatal("a run was started despite the rate limit")
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	good := sign(body, "secret")
	if !verifySignature(body, "secret", good) {
		t.Fatal("verifySignature with the correct HMAC = false, want true")
	}
	if verifySignature(body, "secret", "deadbeef") {
		t.Fatal("verifySignature with a wrong HMAC = true, want false")
	}
	if verifySignature(body, "different-secret", good) {
		t.Fatal("verifySignature with a wrong secret = true, want false")
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
