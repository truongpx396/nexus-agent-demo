// Types mirrored from internal/oversight/rest (handler.go).

// ApprovalView, internal/oversight/rest/handler.go.
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

// ResumeOutcome, internal/oversight/rest/handler.go -- the response for
// grant/deny.
export interface ResumeOutcome {
  session_id: string;
  events_appended: number;
  error?: string;
}

// Decision-ready context for a tool-call approval (handler.go's own doc
// comment: "renders recipient/subject/attachment digests ... never a bare
// UUID"). Other ask_kinds may carry a differently-shaped context -- render
// these three fields when present, fall back to raw JSON otherwise.
export interface ApprovalToolContext {
  tool_id?: string;
  effect_class?: string;
  input?: unknown;
}
