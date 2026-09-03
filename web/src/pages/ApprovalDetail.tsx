import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiError, denyApproval, getApproval, grantApproval } from "../lib/api";
import { useSettings } from "../lib/settings";
import type { ApprovalToolContext, ApprovalView } from "../lib/types";
import { JsonView } from "../components/JsonView";

export function ApprovalDetail() {
  const { id } = useParams<{ id: string }>();
  const { settings, isConfigured } = useSettings();
  const navigate = useNavigate();

  const [approval, setApproval] = useState<ApprovalView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const [showModify, setShowModify] = useState(false);
  const [modifiedInputText, setModifiedInputText] = useState("");
  const [modifyError, setModifyError] = useState<string | null>(null);

  const [denyReason, setDenyReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<string | null>(null);

  const refresh = async () => {
    if (!id || !isConfigured) {
      setLoading(false);
      return;
    }
    try {
      const res = await getApproval(settings, id);
      setApproval(res);
      setError(null);
      setModifiedInputText(formatContextInputSeed(res));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, settings.baseUrl, settings.tenantId, settings.userId]);

  const doGrant = async (withModifiedInput: boolean) => {
    if (!id) return;
    setBusy(true);
    setOutcome(null);
    setModifyError(null);
    try {
      let modified: unknown = undefined;
      if (withModifiedInput) {
        try {
          modified = JSON.parse(modifiedInputText);
        } catch {
          setModifyError("modified input must be valid JSON");
          setBusy(false);
          return;
        }
      }
      const res = await grantApproval(settings, id, modified);
      setOutcome(`granted — session ${res.session_id}, ${res.events_appended} event(s) appended${res.error ? `, error: ${res.error}` : ""}`);
      await refresh();
    } catch (err) {
      setOutcome(null);
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const doDeny = async () => {
    if (!id) return;
    setBusy(true);
    setOutcome(null);
    try {
      const res = await denyApproval(settings, id, denyReason);
      setOutcome(`denied — session ${res.session_id}, ${res.events_appended} event(s) appended${res.error ? `, error: ${res.error}` : ""}`);
      await refresh();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (!id) return <p className="callout error">No approval id in URL.</p>;

  const toolContext = extractToolContext(approval?.context);

  return (
    <div className="page">
      <p>
        <Link to="/approvals">&larr; back to approvals</Link>
      </p>
      <h1>
        Approval <span className="mono">{id}</span>
      </h1>

      {!isConfigured && (
        <p className="callout warn">Set your identity in Settings above to load this approval.</p>
      )}
      {loading && <p className="hint">Loading...</p>}
      {error && <p className="callout error">{error}</p>}

      {approval && (
        <>
          <section className="approval-summary">
            <span className={`status-pill status-${approval.status}`}>{approval.status}</span>
            <span>session: <span className="mono">{approval.session_id}</span></span>
            <span>expires: {formatTime(approval.expires_at)}</span>
            <span>created: {formatTime(approval.created_at)}</span>
          </section>

          <section className="decision-context">
            <h2>Decision-ready context</h2>
            <p className="hint">ask_kind: {approval.ask_kind}</p>
            {toolContext ? (
              <dl className="context-fields">
                {toolContext.tool_id !== undefined && (
                  <>
                    <dt>tool_id</dt>
                    <dd className="mono">{toolContext.tool_id}</dd>
                  </>
                )}
                {toolContext.effect_class !== undefined && (
                  <>
                    <dt>effect_class</dt>
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
              <>
                <p className="hint">
                  Context doesn't match the tool-call shape (tool_id/effect_class/input) — showing
                  raw JSON.
                </p>
                <JsonView value={approval.context} defaultOpen />
              </>
            )}
          </section>

          <section className="approval-actions">
            <h2>Decide</h2>
            {approval.status !== "pending" && (
              <p className="callout warn">
                This approval is already {approval.status}; actions below will likely no-op or
                error.
              </p>
            )}

            <div className="controls-row">
              <button type="button" disabled={busy} onClick={() => doGrant(false)}>
                Grant
              </button>
              <button type="button" className="secondary" disabled={busy} onClick={() => setShowModify((s) => !s)}>
                {showModify ? "hide" : "grant with modified input..."}
              </button>
            </div>

            {showModify && (
              <div className="modify-panel">
                <label>
                  Modified input (JSON)
                  <textarea
                    rows={6}
                    value={modifiedInputText}
                    onChange={(e) => setModifiedInputText(e.target.value)}
                  />
                </label>
                {modifyError && <p className="callout error">{modifyError}</p>}
                <button type="button" disabled={busy} onClick={() => doGrant(true)}>
                  Grant with modified input
                </button>
              </div>
            )}

            <div className="controls-row deny-row">
              <input
                type="text"
                placeholder="reason for denial"
                value={denyReason}
                onChange={(e) => setDenyReason(e.target.value)}
              />
              <button type="button" className="danger" disabled={busy} onClick={doDeny}>
                Deny
              </button>
            </div>

            {outcome && <p className="callout success">{outcome}</p>}
          </section>

          <p>
            <button type="button" className="secondary" onClick={() => navigate(`/runs/${approval.session_id}`)}>
              View run
            </button>
          </p>
        </>
      )}
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

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}
