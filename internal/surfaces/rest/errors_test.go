package rest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerError_AnswersGenericallyAndKeepsTheCauseOutOfTheBody(t *testing.T) {
	cause := errors.New(`ERROR: relation "events" does not exist (SQLSTATE 42P01) at 10.0.0.7:5432`)
	rec := httptest.NewRecorder()

	ServerError(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/x", nil), "load run", cause)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "load run") {
		t.Errorf("body %q does not name the operation that failed", body)
	}
	for _, leaked := range []string{"SQLSTATE", "relation", "10.0.0.7"} {
		if strings.Contains(body, leaked) {
			t.Errorf("body %q leaks %q from the underlying error", body, leaked)
		}
	}
}
