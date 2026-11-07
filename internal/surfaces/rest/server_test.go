package rest_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// pingModule is a Module with one route that reports the principal it was
// handed.
type pingModule struct{}

func (pingModule) Routes(r restkit.Router) {
	r.Handle("GET /ping", func(w http.ResponseWriter, req *http.Request) {
		tenantID, userID, ok := restkit.Principal(w, req)
		if !ok {
			return
		}
		restkit.WriteJSON(w, http.StatusOK, map[string]string{"tenant": tenantID.String(), "user": userID.String()})
	})
}

func TestServer_EveryModuleRouteIsAuthenticated(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	h := restkit.NewServer(resttest.Verifier{TenantID: tenantID, UserID: userID}, pingModule{}).Handler()

	// No token: the module cannot opt out of authentication, because the only
	// way to register a route is Router.Handle.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /ping with no token = %d, want 401", rec.Code)
	}

	rec = resttest.Do(t, h, http.MethodGet, "/ping", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ping with a valid token = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); !strings.Contains(got, tenantID.String()) || !strings.Contains(got, userID.String()) {
		t.Errorf("body %q does not carry the verified principal", got)
	}
}

// An unmounted path answers 404 without asking for a token first: wrapping the
// whole mux in authentication would turn every 404 into a 401 and hide which
// capabilities a deployment has.
func TestServer_AnUnmountedPathIs404NotUnauthorized(t *testing.T) {
	h := restkit.NewServer(resttest.Verifier{}, pingModule{}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nowhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nowhere = %d, want 404", rec.Code)
	}
}

func TestServer_AnswersABrowsersPreflightWithoutAToken(t *testing.T) {
	h := restkit.NewServer(resttest.Verifier{}, pingModule{}).Handler()
	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS /ping = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want the caller's origin", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("a preflight must say which headers are allowed")
	}
}
