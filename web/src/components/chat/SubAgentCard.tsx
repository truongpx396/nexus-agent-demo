import { useState } from "react";
import { JsonView } from "../JsonView";
import type { TimelineItem } from "../../lib/timeline";
import { ChatThread } from "./ChatThread";

type ToolItem = Extract<TimelineItem, { kind: "tool" }>;

interface DelegateInput {
  agent_id?: string;
  task?: string;
}

/** Rendered instead of ToolCallCard for a platform/delegate call once its
 * delegation_requested has told us the child session id
 * (internal/delegate/spawn.go sets the child's user_id to the parent's, so
 * the same bearer token can stream it directly). Collapsed by default; the
 * nested ChatThread is recursive -- a sub-agent that itself delegates gets
 * its own nested SubAgentCard, for free. */
export function SubAgentCard({ item }: { item: ToolItem & { childSessionId: string } }) {
  const [open, setOpen] = useState(false);
  const input = asDelegateInput(item.input);
  const done = Boolean(item.result);
  const isError = item.result?.isError ?? false;

  return (
    <div className={`sub-agent-card ${done ? (isError ? "error" : "done") : "pending"}`}>
      <button type="button" className="sub-agent-head" onClick={() => setOpen((o) => !o)}>
        <span className="sub-agent-icon">🤖</span>
        <span className="sub-agent-title">
          Delegated to <span className="mono">{input.agent_id ?? "sub-agent"}</span>
        </span>
        <span className="tool-call-status">{!done ? "working…" : isError ? "failed" : "returned"}</span>
        <span className="tool-call-toggle">{open ? "hide conversation" : "view conversation"}</span>
      </button>

      {input.task && <div className="sub-agent-task">{input.task}</div>}

      {open && (
        <div className="sub-agent-body">
          <ChatThread runId={item.childSessionId} embedded />
        </div>
      )}

      {done && !open && item.result && (
        <div className="sub-agent-summary">
          <JsonView value={isError ? item.result.reason : item.result.output} />
        </div>
      )}
    </div>
  );
}

function asDelegateInput(input: unknown): DelegateInput {
  if (typeof input !== "object" || input === null) return {};
  const obj = input as Record<string, unknown>;
  return {
    agent_id: typeof obj.agent_id === "string" ? obj.agent_id : undefined,
    task: typeof obj.task === "string" ? obj.task : undefined,
  };
}
