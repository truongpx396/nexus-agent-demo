package anthropic

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The default client must not wait forever for an endpoint that accepts the
// connection and never answers, but it must not cut off a long stream either: the
// bound is on the response headers, not on the whole response.
func TestDefaultClient_BoundsTheWaitForHeadersButNotTheStream(t *testing.T) {
	const headerTimeout = 100 * time.Millisecond
	client := newClient(headerTimeout)
	do := func(t *testing.T, srv *httptest.Server) (string, error) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close() //nolint:errcheck // test
		var sb strings.Builder
		buf := make([]byte, 16)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				return sb.String(), nil
			}
		}
	}

	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	start := time.Now()
	if _, err := do(t, stalled); err == nil {
		t.Fatal("a server that never sends headers did not time out")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v to give up on a server that never answered, want about %v", took, headerTimeout)
	}

	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers are out at once, then the body outlasts the header timeout
		for _, part := range []string{"one ", "two ", "three"} {
			time.Sleep(headerTimeout)
			_, _ = w.Write([]byte(part))
			w.(http.Flusher).Flush()
		}
	}))
	defer streaming.Close()
	if body, err := do(t, streaming); err != nil || body != "one two three" {
		t.Errorf("a stream that outlasts the header timeout = %q, %v; want it delivered whole", body, err)
	}
}

func TestProvider_DefaultsToTheBoundedClientNotTheStdlibOne(t *testing.T) {
	if (&Provider{}).client() == http.DefaultClient {
		t.Error("a Provider with no Client uses http.DefaultClient, which has no timeout of any kind")
	}
	custom := &http.Client{}
	if (&Provider{Client: custom}).client() != custom {
		t.Error("a caller-supplied Client must win")
	}
}
