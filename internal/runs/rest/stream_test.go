package rest

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest/resttest"
)

// A recorder holds everything until the handler returns, so it cannot show that
// the SSE route streams. This test runs the route over a real connection: the
// client gets the replayed frame while the handler is still blocked waiting for
// more (client.Do returns only once the response headers reach it, and they
// leave with the first flush), then gets a live event published afterwards, and
// the stream ends on the terminal one.
func TestEvents_FlushesEachFrameToTheClientAsItIsWritten(t *testing.T) {
	tenantID, userID, runID := uuid.New(), uuid.New(), uuid.New()
	ev := func(seq int64, typ eventlog.EventType) eventlog.Event {
		return eventlog.Event{EventID: uuid.New(), SessionID: runID, TenantID: tenantID, Seq: seq, Type: typ, KeyID: "k", Actor: eventlog.ActorSystem, CreatedAt: time.Now()}
	}
	r := &fakeReader{
		run:    runs.RunView{RunID: runID, UserID: userID},
		events: []eventlog.Event{ev(1, eventlog.EventUserMessage)}, // not terminal: the handler stays open
	}
	h := NewHandler(nil, r)
	srv := httptest.NewServer(restkit.NewServer(resttest.Verifier{TenantID: tenantID, UserID: userID}, h).Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+runID.String()+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+resttest.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("no response headers while the run is still open: the first frame was not flushed (%v)", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}

	in := bufio.NewReader(resp.Body)
	frame := func() string {
		t.Helper()
		var b strings.Builder
		for {
			line, err := in.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended before a whole frame (%q so far): %v", b.String(), err)
			}
			if line == "\n" {
				return b.String()
			}
			b.WriteString(line)
		}
	}

	if first := frame(); !strings.HasPrefix(first, "event: user_message\ndata: ") {
		t.Fatalf("first frame = %q, want the replayed user_message", first)
	}

	// The subscription was made before the first flush, so this live publish cannot be missed.
	h.PublishEvent(tenantID, runID, ev(2, eventlog.EventTerminal), nil)
	if last := frame(); !strings.HasPrefix(last, "event: terminal\ndata: ") {
		t.Fatalf("live frame = %q, want the terminal event", last)
	}
	if rest, err := in.ReadString('\n'); err == nil || rest != "" {
		t.Errorf("stream continued past the terminal event with %q (%v), want it closed", rest, err)
	}
}
