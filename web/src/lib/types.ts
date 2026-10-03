// Types mirrored from internal/surfaces/rest (server.go, oversight.go,
// runctl.go). Kept intentionally loose where the backend itself treats a
// field as an open set (e.g. event `type`) rather than a fixed enum.

export interface Settings {
  baseUrl: string;
  // Bearer token minted out of band (`nexusd token --tenant=<name>`) --
  // replaces the old dev-mode tenantId/userId header pair (README task
  // 13.1). The backend derives tenant/user identity from this token's
  // verified claims, never from anything the client asserts directly.
  token: string;
}

export type Autonomy = "read_only" | "supervised" | "autonomous";

export interface CreateRunRequest {
  input: string;
  data_label?: string;
  difficulty?: string;
  autonomy?: Autonomy | "";
  budget_usd?: string;
  // conversational opts the session into pause-not-terminate semantics
  // (internal/surfaces/rest/run_create.go's own doc comment on
  // createRunRequest.Conversational): a plain reply suspends the session
  // awaiting the next message ("awaiting_input" status) instead of ending
  // the run, and POST .../steer transparently resumes it in place. The web
  // chat UI always sets this true.
  conversational?: boolean;
}

export interface CreateRunResponse {
  run_id: string;
}

export interface GetRunResponse {
  run_id: string;
  status: string;
  terminal_reason?: string;
}

// eventDTO, internal/surfaces/rest/server.go. `type` is deliberately an open
// string set on the wire (turn_started, tool_use, tool_result,
// approval_requested, terminal, error, ...) -- don't hardcode an exhaustive
// union here.
export interface RunEvent {
  event_id: string;
  seq: number;
  type: string;
  actor: string;
  tool_id?: string;
  pair_ref?: string;
  model_id?: string;
  created_at: string;
  body?: unknown;
}

// DeltaDTO, internal/surfaces/rest/broker.go -- a live, best-effort preview
// chunk sent as its own `event: delta` SSE frame, never replayed from
// history and never carrying an event_id/seq the way RunEvent does (that's
// what makes it structurally distinguishable from one). Kind == "reasoning"
// never carries text -- a signal that the model is reasoning right now,
// never its content (kernel.Kernel.OnChunk's own doc comment).
export interface DeltaFrame {
  kind: "content" | "tool_use" | "reasoning";
  text?: string;
  tool_use_id?: string;
  tool_name?: string;
}

// sessionSummary, internal/surfaces/rest/sessions_list.go -- one row of
// GET /v1/sessions (the web UI's session sidebar).
export interface SessionSummary {
  run_id: string;
  status: string;
  terminal_reason?: string;
  created_at: string;
  autonomy_level: string;
}

// ApprovalView, internal/surfaces/rest/oversight.go.
export interface ApprovalView {
  approval_id: string;
  session_id: string;
  tool_id: string;
  ask_kind: string;
  status: string;
  context: unknown;
  expires_at: string;
  created_at: string;
}

// ResumeOutcome, internal/surfaces/rest/oversight.go -- the response for
// grant/deny.
export interface ResumeOutcome {
  session_id: string;
  events_appended: number;
  error?: string;
}

// ForkView, internal/surfaces/rest/runctl.go.
export interface ForkView {
  session_id: string;
  digest_diverged: boolean;
  parent_digest_hex: string;
  child_digest_hex: string;
}

// Decision-ready context for a tool-call approval (oversight.go's own doc
// comment: "renders recipient/subject/attachment digests ... never a bare
// UUID"). Other ask_kinds may carry a differently-shaped context -- render
// these three fields when present, fall back to raw JSON otherwise.
export interface ApprovalToolContext {
  tool_id?: string;
  effect_class?: string;
  input?: unknown;
}
