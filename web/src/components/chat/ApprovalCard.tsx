import { useState } from "react";
import { ApiError, denyApproval, grantApproval } from "../../lib/api";
import { useSettings } from "../../lib/settings";
import { JsonView } from "../JsonView";
import type { ApprovalToolContext, ApprovalView } from "../../lib/types";

/** The generic tool-call approval prompt -- ported from ApprovalDetail.tsx's
 * decision-ready rendering, inlined into the chat thread instead of a
 * separate page. Rendered for every pending approval except
 * platform/ask_clarification's (see ClarificationCard). */
export function ApprovalCard({ approval, onResolved }: { approval: ApprovalView; onResolved: () => void }) {
  const { settings } = useSettings();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showModify, setShowModify] = useState(false);
  const [modifiedInputText, setModifiedInputText] = useState(() => formatContextInputSeed(approval));
  const [denyReason, setDenyReason] = useState("");

  const toolContext = extractToolContext(approval.context);

  const doGrant = async (withModifiedInput: boolean) => {
    setBusy(true);
    setError(null);
    try {
      let modified: unknown = undefined;
      if (withModifiedInput) {
        try {
          modified = JSON.parse(modifiedInputText);
        } catch {
          setError("modified input must be valid JSON");
          setBusy(false);
          return;
        }
      }
      await grantApproval(settings, approval.approval_id, modified);
      onResolved();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const doDeny = async () => {
    setBusy(true);
    setError(null);
    try {
      await denyApproval(settings, approval.approval_id, denyReason);
      onResolved();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="approval">
      <div className="approval-question">
        ⏸ Approve this action?{" "}
        {toolContext?.tool_id && <span className="mono">{toolContext.tool_id}</span>}
      </div>

      {toolContext ? (
        <dl className="context-fields">
          {toolContext.effect_class !== undefined && (
            <>
              <dt>effect</dt>
              <dd className="mono">{toolContext.effect_class}</dd>
            </>
          )}
          {toolContext.input !== undefined && (
            <>
              <dt>input</dt>
              <dd>
                <JsonView value={toolContext.input} defaultOpen />
              </dd>
            </>
          )}
        </dl>
      ) : (
        <JsonView value={approval.context} defaultOpen />
      )}

      <div className="followups approval-actions-row">
        <button type="button" disabled={busy} onClick={() => doGrant(false)}>
          Approve
        </button>
        <button type="button" className="secondary" disabled={busy} onClick={() => setShowModify((s) => !s)}>
          {showModify ? "hide" : "approve with edited input…"}
        </button>
        <button type="button" className="danger" disabled={busy} onClick={doDeny}>
          Deny
        </button>
      </div>

      {showModify && (
        <div className="modify-panel">
          <textarea rows={5} value={modifiedInputText} onChange={(e) => setModifiedInputText(e.target.value)} />
          <button type="button" disabled={busy} onClick={() => doGrant(true)}>
            Approve with this input
          </button>
        </div>
      )}

      <input
        type="text"
        className="deny-reason-input"
        placeholder="reason for denial (optional)"
        value={denyReason}
        onChange={(e) => setDenyReason(e.target.value)}
      />

      {error && <p className="callout error">{error}</p>}
    </div>
  );
}

function extractToolContext(context: unknown): ApprovalToolContext | null {
  if (typeof context !== "object" || context === null || Array.isArray(context)) return null;
  const obj = context as Record<string, unknown>;
  if (!("tool_id" in obj) && !("effect_class" in obj) && !("input" in obj)) return null;
  return {
    tool_id: typeof obj.tool_id === "string" ? obj.tool_id : undefined,
    effect_class: typeof obj.effect_class === "string" ? obj.effect_class : undefined,
    input: obj.input,
  };
}

function formatContextInputSeed(approval: ApprovalView): string {
  const ctx = extractToolContext(approval.context);
  if (ctx && ctx.input !== undefined) {
    try {
      return JSON.stringify(ctx.input, null, 2);
    } catch {
      return "";
    }
  }
  return "";
}
