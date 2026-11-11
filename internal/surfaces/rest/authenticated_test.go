package rest_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// generatedModule mounts the way a generated server does: on the router
// Authenticated returns, with restkit.ParamError for a parameter that does not
// parse (what a feature's Routes passes in GinServerOptions).
type generatedModule struct{}

func (generatedModule) Routes(r restkit.Router) {
	g := r.Authenticated()
	g.POST("/gen", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			restkit.Error(c, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})
	g.GET("/gen/:id", func(c *gin.Context) {
		if _, err := uuid.Parse(c.Param("id")); err != nil {
			restkit.ParamError(c, err, http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})
}

// A route registered on Authenticated is as closed and as bounded as one
// registered with GET or POST, and an unmounted path still answers 404 first.
func TestRouter_AuthenticatedRoutesNeedATokenAndBoundTheirBody(t *testing.T) {
	h := restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: uuid.New()}, generatedModule{}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/gen", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /gen with no token = %d, want 401", rec.Code)
	}
	if rec := resttest.Do(t, h, http.MethodPost, "/gen", strings.Repeat("a", 5<<20)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversize body on an authenticated route = %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nowhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unmounted path = %d, want 404 and not 401", rec.Code)
	}
}

func TestParamError_AnswersTheMessageAsPlainTextWithTheGivenStatus(t *testing.T) {
	h := restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: uuid.New()}, generatedModule{}).Handler()
	rec := resttest.Do(t, h, http.MethodGet, "/gen/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), "invalid UUID") {
		t.Errorf("a bad parameter = %d %q (%s), want 400 and a one-line plain-text message", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
}

func TestServer_RoutesListsEveryMountedRouteInOpenAPISyntax(t *testing.T) {
	routes := restkit.NewServer(nil, generatedModule{}).Routes()
	want := map[restkit.Route]bool{{Method: "POST", Path: "/gen"}: true, {Method: "GET", Path: "/gen/{id}"}: true}
	if len(routes) != len(want) {
		t.Fatalf("routes = %v, want %v", routes, want)
	}
	for _, r := range routes {
		if !want[r] {
			t.Errorf("unexpected route %v", r)
		}
	}
}
