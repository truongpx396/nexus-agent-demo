package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// adapter wraps one remote MCP tool as an ordinary tools.Tool (README task
// 11.1: "No new tool ABI; an external tool is exactly as trusted as a
// builtin one, never more"). Every method here is a thin translation —
// there is no special-casing anywhere else in the pipeline for an
// mcp-namespaced ref; internal/tools/pipeline.go's resolveTool only knows
// it got SOME Tool back from cfg.Dynamic.
type adapter struct {
	ref      tools.ToolRef
	remote   Tool
	client   *Client
	deniedBy string // set by Resolver when the server itself is admitted but its status forbids a call anyway; empty in the ordinary case
}

func (a *adapter) ID() tools.ToolRef { return a.ref }

func (a *adapter) Descriptor() tools.Descriptor {
	schema := a.remote.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return tools.Descriptor{
		ID:          a.ref,
		Description: a.remote.Description,
		InputSchema: schema,
		EffectClass: tools.EffectClassExternal,
	}
}

// Taint: DefaultTaint (all-true) — an external, tenant-admitted-but-
// unaudited remote server is never trusted narrower than the fail-closed
// floor every under-declared tool gets (tools.DefaultTaint's own doc
// comment), regardless of what the server's own description claims about
// itself.
func (a *adapter) Taint() tools.Taint { return tools.DefaultTaint() }

func (a *adapter) IsConcurrencySafe(json.RawMessage) bool { return false } // an unaudited remote effect is never assumed safe to race

func (a *adapter) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	if a.deniedBy != "" {
		return tools.PermissionResult{Decision: "deny", Reason: a.deniedBy}
	}
	return tools.PermissionResult{Decision: "defer"} // no MCP-specific carve-out — the chain decides, same as every other tool
}

func (a *adapter) ValidateInput(_ context.Context, in json.RawMessage, _ tools.RunContext) error {
	// A full JSON-Schema validator is out of scope (README task 11.1's own
	// plan: no such dependency exists in this codebase today, and adding
	// one for this alone isn't justified) — this only checks the input is
	// well-formed JSON, matching what internal/tools/pipeline.go's step 4
	// actually needs before binding a canonical digest over it.
	if len(in) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	return nil
}

func (a *adapter) Call(ctx context.Context, in json.RawMessage, _ tools.RunContext) (tools.Result, error) {
	res, err := a.client.CallTool(ctx, a.remote.Name, in)
	if err != nil {
		return tools.Result{IsError: true, Reason: err.Error()}, nil
	}
	out, err := json.Marshal(res)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: out, IsError: res.IsError}, nil
}
