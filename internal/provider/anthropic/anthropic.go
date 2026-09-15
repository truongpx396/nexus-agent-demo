// Package anthropic is the one real hosted-model adapter this demo ships
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
	Client    *http.Client // defaults to http.DefaultClient
	MaxTokens int          // defaults to defaultMaxTokens
}

func New(apiKey, model string) *Provider {
	return &Provider{APIKey: apiKey, Model: model}
}

func (p *Provider) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return defaultBaseURL
}

func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Provider) maxTokens() int {
	if p.MaxTokens != 0 {
		return p.MaxTokens
	}
	return defaultMaxTokens
}
