//go:build integration

// README task 13.14 (closing production-readiness finding F14: "every
// scale knob is at its zero value, and nothing measures the ceiling"): one
// load test against POST /v1/runs on provider/fake, through the SAME real
// Postgres + PgBouncer (transaction pooling) topology every other
// integration test in this package already uses, to put a measured number
// on the queue/pooler ceiling rather than an assumed one.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/authn"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// loadTestConcurrency/loadTestTotalRequests are deliberately modest — this
// proves the request path holds up under concurrent load and reports a
// real throughput number, not a stress test meant to find a breaking
// point (that's a job for a dedicated k6/vegeta run against a real
// deployment, which this in-process test can't stand in for).
const (
	loadTestConcurrency   = 20
	loadTestTotalRequests = 200
)

func TestLoadPOSTRunsThroughput(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	signingKey, err := authn.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate test signing key: %v", err)
	}
	token := mustIssueToken(t, authn.NewDevIssuer(signingKey), tenantID, uuid.New())

	// One fresh fake.Provider per request: each backs exactly one
	// Stream() call with a short scripted completion, so the harness
	// measures the REST/kernel/store/PgBouncer path's own throughput
	// ceiling, not a shared fake stream's internal state.
	newStarter := func() *testRunStarter {
		script := fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "done"},
			{Kind: "usage", InputUncached: 10, OutputTokens: 2},
			{Kind: "done", Done: "stop"},
		}}
		return &testRunStarter{kernel: &kernel.Kernel{
			Provider: provider.Wrap([]provider.Provider{fake.New(script)}),
			Tools:    kernel.NotImplementedToolExecutor{},
			Budget:   kernel.NoopBudgetGate{},
			Store:    st,
		}}
	}

	srv := rest.NewServer(newStarter(), st, keyStore, nil)
	srv.Verifier = authn.NewDevVerifier(authn.PublicKey(signingKey))
	srv.ControlPlane = newTestControlPlane(st)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := httpSrv.Client()

	var succeeded, failed int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, loadTestConcurrency)
	start := time.Now()

	for i := 0; i < loadTestTotalRequests; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs",
				strings.NewReader(fmt.Sprintf(`{"input":"load test request %d"}`, i)))
			if err != nil {
				atomic.AddInt64(&failed, 1)
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("content-type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&failed, 1)
				return
			}
			defer resp.Body.Close() //nolint:errcheck // read-only response body in a load test
			if resp.StatusCode == http.StatusAccepted {
				atomic.AddInt64(&succeeded, 1)
			} else {
				atomic.AddInt64(&failed, 1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	throughput := float64(loadTestTotalRequests) / elapsed.Seconds()
	t.Logf("load test: %d requests at concurrency %d in %s (%.1f req/s), %d succeeded, %d failed",
		loadTestTotalRequests, loadTestConcurrency, elapsed, throughput, succeeded, failed)

	if failed > 0 {
		t.Errorf("%d/%d requests failed (want 0) — see log for measured throughput", failed, loadTestTotalRequests)
	}
	if succeeded != loadTestTotalRequests {
		t.Errorf("succeeded = %d, want %d", succeeded, loadTestTotalRequests)
	}
}
