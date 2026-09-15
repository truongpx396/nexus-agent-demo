import { useState } from "react";
import { JsonView } from "../JsonView";
import type { TimelineItem } from "../../lib/timeline";
import { CitationsPanel } from "./CitationsPanel";

type ToolItem = Extract<TimelineItem, { kind: "tool" }>;

/** One tool_use/tool_result pair -- pending (still running or awaiting a
 * human decision), or done (ok/error). */
export function ToolCallCard({ item }: { item: ToolItem }) {
  const [open, setOpen] = useState(false);
  const done = Boolean(item.result);
  const isError = item.result?.isError ?? false;

  const icon = !done ? (item.awaitingApproval ? "⏸" : "🔧") : isError ? "✗" : "✓";
  const stateClass = !done ? (item.awaitingApproval ? "awaiting" : "pending") : isError ? "error" : "done";

  const isRetrieve = item.toolId === "platform/retrieve" && done && !isError;

  return (
    <div className={`tool-call ${stateClass}`}>
      <button type="button" className="tool-call-head" onClick={() => setOpen((o) => !o)}>
        <span className="tool-call-icon">{icon}</span>
        <span className="tool-call-id mono">{item.toolId}</span>
        <span className="tool-call-status">
          {!done ? (item.awaitingApproval ? "waiting for approval" : "running…") : isError ? "failed" : "done"}
        </span>
        <span className="tool-call-toggle">{open ? "hide details" : "details"}</span>
      </button>

      {isRetrieve && <CitationsPanel output={item.result?.output} />}

      {open && (
        <div className="tool-call-details">
          <div className="tool-call-field">
            <div className="tool-call-field-label">input</div>
            <JsonView value={item.input} />
          </div>
          {done && (
            <div className="tool-call-field">
              <div className="tool-call-field-label">{isError ? "error" : "output"}</div>
              <JsonView value={isError ? item.result?.reason : item.result?.output} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}
