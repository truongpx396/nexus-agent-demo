package rest_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// echoModule mounts one route that decodes its body with the kit's helper.
type echoModule struct{}

func (echoModule) Routes(r restkit.Router) {
	r.POST("/echo", func(c *gin.Context) {
		var body struct {
			Text string `json:"text"`
		}
		if !restkit.DecodeJSON(c, &body) {
			return
		}
		c.JSON(http.StatusOK, map[string]string{"text": body.Text})
	})
	// A handler that ignores DecodeJSON and reads the body itself is bounded too.
	r.POST("/raw", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			restkit.Error(c, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})
}

func TestDecodeJSON_AnswersEachFailureWithItsOwnStatus(t *testing.T) {
	h := restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: uuid.New()}, echoModule{}).Handler()

	if rec := resttest.Do(t, h, http.MethodPost, "/echo", `{"text":"hi"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hi") {
		t.Errorf("a valid body = %d %q, want 200 echoing it", rec.Code, rec.Body)
	}
	if rec := resttest.Do(t, h, http.MethodPost, "/echo", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed body = %d, want 400", rec.Code)
	}
	huge := `{"text":"` + strings.Repeat("a", 5<<20) + `"}` // past the 4 MiB limit
	if rec := resttest.Do(t, h, http.MethodPost, "/echo", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversize body = %d, want 413: it must be refused, and not buffered or called invalid JSON", rec.Code)
	}
}

func TestRouter_BoundsTheBodyOfEveryRoute(t *testing.T) {
	h := restkit.NewServer(resttest.Verifier{TenantID: uuid.New(), UserID: uuid.New()}, echoModule{}).Handler()
	if rec := resttest.Do(t, h, http.MethodPost, "/raw", strings.Repeat("a", 5<<20)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a handler that reads the body itself got %d on an oversize body, want 413", rec.Code)
	}
}
