package rest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// fakeVerifier is a minimal PrincipalVerifier for testing authMiddleware in
// isolation — accepts exactly one token string, rejects everything else.
type fakeVerifier struct {
	wantToken        string
	tenantID, userID uuid.UUID
}

func (f fakeVerifier) Verify(token string) (uuid.UUID, uuid.UUID, error) {
	if token != f.wantToken {
		return uuid.Nil, uuid.Nil, errors.New("invalid token")
	}
	return f.tenantID, f.userID, nil
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	s := &Server{Verifier: fakeVerifier{wantToken: "good"}}
	h := s.authed(func(http.ResponseWriter, *http.Request) { t.Fatal("handler reached with no Authorization header") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing Authorization header: got %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	s := &Server{Verifier: fakeVerifier{wantToken: "good"}}
	h := s.authed(func(http.ResponseWriter, *http.Request) { t.Fatal("handler reached with an invalid token") })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: got %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_NilVerifier(t *testing.T) {
	s := &Server{}
	h := s.authed(func(http.ResponseWriter, *http.Request) { t.Fatal("handler reached with no Verifier wired") })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer anything")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil Verifier: got %d, want 500", rec.Code)
	}
}

func TestAuthMiddleware_ValidTokenReachesHandlerWithPrincipal(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	s := &Server{Verifier: fakeVerifier{wantToken: "good", tenantID: tenantID, userID: userID}}
	reached := false
	h := s.authed(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		gotTenant, gotUser, ok := principalFromContext(r.Context())
		if !ok || gotTenant != tenantID || gotUser != userID {
			t.Errorf("principal in context = (%v,%v,%v), want (%v,%v,true)", gotTenant, gotUser, ok, tenantID, userID)
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer good")
	h.ServeHTTP(rec, req)
	if !reached {
		t.Fatal("valid token did not reach the wrapped handler")
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid token was rejected: got 401")
	}
}

func TestAuthMiddleware_UnregisteredRouteIs404NotAuthGated(t *testing.T) {
	// A route Handler() never mounts must fall through to the mux's own
	// native 404 without needing a token at all — this is what lets
	// capability_test.go's "404 means unmounted" conformance-test technique
	// keep working unmodified after auth was added to every registered
	// route (server.go's Handler(), authed()'s own doc comment).
	s := &Server{Verifier: fakeVerifier{wantToken: "good"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered route: got %d, want 404", rec.Code)
	}
}
