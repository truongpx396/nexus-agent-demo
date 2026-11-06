package hooks

import "encoding/json"

// Request and Response are the JSON a command hook reads on stdin / writes on
// stdout, and an http hook POSTs / receives as its body: one wire shape for
// both transports, defined here so the transports (internal/hooks/exechook,
// internal/hooks/httphook) stay thin adapters over the same protocol.
type Request struct {
	Event  string          `json:"event"`
	ToolID string          `json:"tool_id"`
	Input  json.RawMessage `json:"input"`
}

// Response is a hook's answer. Decision is a raw string on purpose: normalize
// (dispatcher.go) is what decides whether it is a decision the chain accepts.
type Response struct {
	Decision     string          `json:"decision"`
	Reason       string          `json:"reason,omitempty"`
	UpdatedInput json.RawMessage `json:"updated_tool_input,omitempty"`
}
