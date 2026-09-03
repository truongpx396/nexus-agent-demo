// Package mcp is the MCP client adapter (README Phase 11, task 11.1): each
// remote MCP server's tools are qualified as "mcp/{server}/{tool}@{version}"
// and admitted through the ordinary identity (#13), manifest (#14), and
// descriptor-scan (#15) path — an external tool is exactly as trusted as a
// builtin one, never more. This package never imports kernel/
// (tests/contract/boundaries_test.go's wildcard rule on internal/surfaces/...
// already covers it) and holds no agent control flow of its own — it is a
// hand-rolled JSON-RPC 2.0 client over net/http (MCP's Streamable HTTP
// transport), matching internal/surfaces/rest's own zero-framework style;
// no SDK dependency was added for it.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Tool is one remote MCP server's own self-description of a tool it offers
// — the wire shape MCP's tools/list result carries.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// SchemaDigest is the content-addressed identity adapter.go's ToolRef.Version
// is built from (README task 11.1's own "#15 digest re-verification at use"
// applied to a source that isn't a static bundle, per
// internal/tools/pipeline.go's resolveTool doc comment): a schema change
// produces a DIFFERENT qualified ref, which is a fresh admission decision,
// not a drifted one.
func (t Tool) SchemaDigest() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "name=%s\ndescription=%s\ninput_schema=%s\n", t.Name, t.Description, string(t.InputSchema))
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}

// CallResult is what tools/call returns — Content is untrusted external
// text by construction (Taint().ReturnsUntrusted is always true for an MCP
// tool, adapter.go), IsError marks a tool-level failure the remote server
// reported rather than a transport failure.
type CallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// Client is a JSON-RPC 2.0 client for one remote MCP server's Streamable
// HTTP endpoint. One Client per admitted mcp_servers row.
type Client struct {
	BaseURL     string
	BearerToken string // static token (auth_kind='bearer_static') or a live OAuth access token (auth_kind='oauth_connector'), injected by the caller per request — never cached here
	HTTPClient  *http.Client
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func (c *Client) do(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("mcp: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mcp: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if c.BearerToken != "" {
		req.Header.Set("authorization", "Bearer "+c.BearerToken)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: request %s: %w", method, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body; nothing to flush

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("mcp: decode response for %s (http status %d): %w", method, resp.StatusCode, err)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("mcp: %s refused: %s (code %d)", method, rpcResp.Error.Message, rpcResp.Error.Code)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rpcResp.Result, out); err != nil {
		return fmt.Errorf("mcp: unmarshal result for %s: %w", method, err)
	}
	return nil
}

// Initialize performs MCP's handshake — called once before the first
// ListTools/CallTool against a freshly-constructed Client.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nexus-agent-demo", "version": "1"},
	}
	return c.do(ctx, "initialize", params, nil)
}

// ListTools returns the server's currently-offered tool set.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.do(ctx, "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool invokes name with arguments and returns the server's result.
// The result is untrusted external content by construction — callers never
// feed it back into anything that executes it as instructions.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	var args any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return CallResult{}, fmt.Errorf("mcp: unmarshal arguments: %w", err)
		}
	} else {
		args = map[string]any{}
	}
	var out CallResult
	if err := c.do(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		return CallResult{}, err
	}
	return out, nil
}
