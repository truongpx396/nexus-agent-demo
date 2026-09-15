package anthropic

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type sseEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage sseUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
		StopDetails struct {
			Category string `json:"category"`
		} `json:"stop_details"`
	} `json:"delta"`
	Usage sseUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type errorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// classifyAPIError turns an Anthropic error payload into the typed error
// internal/provider/failover.go's ClassifyTrigger already knows how to read
// — this adapter doesn't reimplement the taxonomy, it just produces the
// right sentinel type at the boundary.
func classifyAPIError(status int, body errorBody) error {
	msg := body.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("http %d", status)
	}
	switch {
	case strings.Contains(strings.ToLower(msg), "too long") || strings.Contains(strings.ToLower(msg), "context"):
		return &provider.ContextOverflowError{Reason: msg}
	case status == http.StatusTooManyRequests, status >= 500, body.Error.Type == "overloaded_error", body.Error.Type == "rate_limit_error":
		return &provider.ThrottleError{Reason: msg}
	default:
		return fmt.Errorf("anthropic: %s: %s", body.Error.Type, msg)
	}
}
