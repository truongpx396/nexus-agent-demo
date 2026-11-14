package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/reporting"
)

// countingReports counts how often a render reads from storage.
type countingReports struct {
	mu    sync.Mutex
	calls int
}

func (c *countingReports) GoldenSignals(context.Context, uuid.UUID, time.Duration) (reporting.GoldenSignals, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return reporting.GoldenSignals{StuckRate: 0.25}, nil
}

func (c *countingReports) ToolCallCounts(context.Context, uuid.UUID) (reporting.ToolCallCounts, error) {
	return reporting.ToolCallCounts{"platform/echo@v1": 3}, nil
}

func (c *countingReports) ModelSpend(context.Context, uuid.UUID) ([]reporting.ModelSpend, error) {
	return nil, nil
}

func (c *countingReports) renders() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func oneTenant(context.Context) ([]uuid.UUID, error) { return []uuid.UUID{uuid.New()}, nil }

func scrape(h http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// /metrics lists every tenant's id, spend and tool names: not for whoever can reach the port.
func TestMetrics_NeedsTheToken(t *testing.T) {
	reports := &countingReports{}
	h := requireBearer("s3cret-token", newMetricsHandler(oneTenant, reports, nil))

	for name, authorization := range map[string]string{
		"no header":          "",
		"the wrong token":    "Bearer wrong",
		"a prefix of it":     "Bearer s3cret",
		"the token + suffix": "Bearer s3cret-token-and-more",
		"another scheme":     "Basic s3cret-token",
		"no scheme":          "s3cret-token",
	} {
		rec := scrape(h, authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "nexus_") {
			t.Errorf("%s: an unauthorised response carried metrics", name)
		}
	}
	if reports.renders() != 0 {
		t.Fatalf("an unauthorised scrape rendered %d times: it must not load the database", reports.renders())
	}

	rec := scrape(h, "Bearer s3cret-token")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nexus_stuck_rate") {
		t.Fatalf("with the token: status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content-type = %q", got)
	}
	if rec := scrape(h, "bearer s3cret-token"); rec.Code != http.StatusOK {
		t.Errorf("a lower-case scheme is still the bearer scheme: status = %d", rec.Code)
	}
}

// Under --dev the token is optional and /metrics stays open for the local scrape.
func TestRequireBearer_AnEmptyTokenPassesThrough(t *testing.T) {
	h := requireBearer("", newMetricsHandler(oneTenant, &countingReports{}, nil))
	if rec := scrape(h, ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no token configured", rec.Code)
	}
}

// Each render lists the tenants and queries storage for each: two scrapes inside the
// window share one.
func TestMetrics_ScrapesInsideTheCacheWindowShareOneRender(t *testing.T) {
	reports := &countingReports{}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	h := newMetricsHandler(oneTenant, reports, nil)
	h.now = func() time.Time { return now }

	first := scrape(h, "")
	now = now.Add(10 * time.Second)
	second := scrape(h, "")
	if reports.renders() != 1 {
		t.Fatalf("two scrapes 10s apart rendered %d times, want 1", reports.renders())
	}
	if first.Body.String() != second.Body.String() || first.Body.Len() == 0 {
		t.Fatal("the second scrape did not get the cached body")
	}

	now = now.Add(6 * time.Second) // 16s after the first
	scrape(h, "")
	if reports.renders() != 2 {
		t.Fatalf("a scrape after the window rendered %d times in total, want 2", reports.renders())
	}
}

func TestMetrics_AFailedRenderIsAnErrorWithoutTheCauseAndIsNotCached(t *testing.T) {
	fail := true
	tenants := func(context.Context) ([]uuid.UUID, error) {
		if fail {
			return nil, errors.New("password authentication failed for user nexus")
		}
		return []uuid.UUID{uuid.New()}, nil
	}
	h := newMetricsHandler(tenants, &countingReports{}, nil)

	rec := scrape(h, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "password") || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("the response is %q, want \"internal error\" and nothing of the cause", rec.Body.String())
	}

	fail = false
	if rec := scrape(h, ""); rec.Code != http.StatusOK {
		t.Fatalf("after the outage: status = %d, want a fresh render, not the cached failure", rec.Code)
	}
}

// A client that hangs up part-way must not leave a body with the tenants missing
// cached for everyone else.
func TestMetrics_ARenderIsNotCancelledWithTheScrapeThatStartedIt(t *testing.T) {
	reports := &countingReports{}
	h := newMetricsHandler(oneTenant, reports, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "nexus_stuck_rate") {
		t.Fatalf("a scrape whose client had gone produced %q, want a complete render", rec.Body.String())
	}
}
