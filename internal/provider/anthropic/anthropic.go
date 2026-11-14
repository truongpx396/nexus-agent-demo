// Package anthropic is the one real hosted-model adapter Nexus Agent ships
// (README task 2.8) against internal/provider.Provider — plain net/http and
// SSE parsing, no vendor SDK, because "all provider access MUST go through
// one internal abstraction" (constitution Principle VII) means the wire
// protocol lives here and nowhere else. It is never exercised by a
// correctness test (Principle IX: those run only against
// internal/provider/fake); anthropic_test.go proves the SSE parsing against
// a canned httptest.Server body, not a live call.
package anthropic

import (
	"net/http"
	"time"

	"github.com/truongpx396/nexus-agent/internal/provider"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 4096
)

// Provider implements provider.Provider against POST /v1/messages with
// stream: true. One Provider value is bound to one model — model selection
// across calls is internal/provider/router.go's job (README task 2.8), not
// this adapter's.
type Provider struct {
	APIKey    string
	Model     string
	BaseURL   string       // defaults to defaultBaseURL
	Client    *http.Client // defaults to a client with a response-header timeout (defaultClient)
	MaxTokens int          // defaults to defaultMaxTokens
}

var _ provider.ModelBound = (*Provider)(nil)

// BoundModel implements provider.ModelBound: every call goes to p.Model.
func (p *Provider) BoundModel() string { return p.Model }

func New(apiKey, model string) *Provider {
	return &Provider{APIKey: apiKey, Model: model}
}

func (p *Provider) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return defaultBaseURL
}

// responseHeaderTimeout bounds how long the API may take to start answering. It
// does not bound the response: headers arrive as generation begins, and a total
// http.Client.Timeout would cut a long stream off mid-sentence. Without any
// bound, an endpoint that accepts the connection and never answers would hold a
// run's worker, and the session lock that worker keeps renewing, indefinitely.
const responseHeaderTimeout = 90 * time.Second

// defaultClient is what a Provider with no Client uses. It is one client for the
// process so its connections are pooled.
var defaultClient = newClient(responseHeaderTimeout)

// newClient is the default transport with a response-header timeout added.
func newClient(headerTimeout time.Duration) *http.Client {
	tr := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone()
	}
	tr.ResponseHeaderTimeout = headerTimeout
	return &http.Client{Transport: tr}
}

func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return defaultClient
}

func (p *Provider) maxTokens() int {
	if p.MaxTokens != 0 {
		return p.MaxTokens
	}
	return defaultMaxTokens
}
