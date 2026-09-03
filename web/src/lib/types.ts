// Types mirrored from internal/surfaces/rest (server.go, oversight.go,
// runctl.go). Kept intentionally loose where the backend itself treats a
// field as an open set (e.g. event `type`) rather than a fixed enum.

export interface Settings {
  baseUrl: string;
  tenantId: string;
  userId: string;
}

export type Autonomy = "read_only" | "supervised" | "autonomous";

export interface CreateRunRequest {
  input: string;
  data_label?: string;
  difficulty?: string;
  autonomy?: Autonomy | "";
  budget_usd?: string;
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
