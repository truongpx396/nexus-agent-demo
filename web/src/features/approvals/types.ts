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

// DecisionResponse, internal/oversight/rest/handler.go -- the response for
// grant/deny. The decision is recorded when it arrives; the run then resumes on a
// queue worker, so there is no count of events. "decided" means the hand-off to the
// queue failed and recovery will resume the run.
export interface DecisionResponse {
  approval_id: string;
  session_id: string;
  status: "resuming" | "decided";
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
