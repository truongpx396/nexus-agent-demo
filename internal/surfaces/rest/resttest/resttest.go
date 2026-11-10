// Package resttest is the test kit for REST modules: a verifier that accepts one
// token, and a request helper that sends it. A feature's rest package uses it to
// drive its handlers through the real kit (authentication, routing) without a
// token issuer or a network.
package resttest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Token is the bearer token Verifier accepts.
const Token = "test-token"

// Verifier accepts exactly Token and resolves it to one principal.
type Verifier struct {
	TenantID, UserID uuid.UUID
}

// Verify implements rest.PrincipalVerifier.
func (v Verifier) Verify(token string) (tenantID, userID uuid.UUID, err error) {
	if token != Token {
		return uuid.Nil, uuid.Nil, errors.New("invalid token")
	}
	return v.TenantID, v.UserID, nil
}

// Do sends an authenticated request with body (empty for none) to h and returns
// the recorded response.
func Do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequestWithContext(t.Context(), method, path, nil)
	} else {
		req = httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
