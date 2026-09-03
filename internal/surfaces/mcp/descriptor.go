package mcp

import "github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"

// Descriptor declares this "surface" for task 11.8's conformance suite.
// MCP is a tool SOURCE, not a principal-submitting surface — it never
// receives an inbound turn, renders approval context, or streams events of
// its own; a run that happens to call an MCP-sourced tool is still
// rendered/approved through whatever surface actually submitted that run
// (REST, Telegram, ...). Every capability bit is therefore its zero value
// except SurfaceID; mcp_test.go's conformance test asserts the resolver-
// level properties this surface actually has instead (a digest mismatch is
// refused, an unadmitted server is never resolved).
var Descriptor = capability.Descriptor{
	SurfaceID:     "mcp",
	PrincipalKind: "", // never resolves a principal of its own
}
