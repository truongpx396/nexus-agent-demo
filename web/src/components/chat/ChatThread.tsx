import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ApiError, cancelRun, createRun, getRun, listApprovals, steerRun } from "../../lib/api";
import { renderMarkdown } from "../../lib/markdown";
import { addRecentRun } from "../../lib/recentRuns";
import { useSettings } from "../../lib/settings";
import { suggestedPrompts } from "../../lib/suggestedPrompts";
import { buildTimeline, findAwaitingApproval, isThinking } from "../../lib/timeline";
import type { Autonomy, ApprovalView, GetRunResponse } from "../../lib/types";
import { useRunEvents } from "../../lib/useRunEvents";
import { ApprovalCard } from "./ApprovalCard";
import { ClarificationCard } from "./ClarificationCard";
import { MessageBubble } from "./MessageBubble";
import { SubAgentCard } from "./SubAgentCard";
import { ToolCallCard } from "./ToolCallCard";

const POLL_INTERVAL_MS = 3000;
const TERMINAL_STATUSES = new Set(["completed", "failed", "cancelled", "canceled", "aborted"]);
const ASK_CLARIFICATION_TOOL_ID = "platform/ask_clarification";

interface Props {
  runId?: string;
  /** Nested sub-agent view (SubAgentCard): no composer, no page chrome --
   * still fully interactive for a pending approval/clarification, since the
   * same principal owns the child session (internal/delegate/spawn.go sets
   * its user_id to the parent's). */
  embedded?: boolean;
}

export function ChatThread({ runId, embedded }: Props) {
  if (!runId) return <ComposeOnly />;
  return <LiveThread runId={runId} embedded={Boolean(embedded)} />;
}

function ComposeOnly() {
  const { settings, isConfigured } = useSettings();
  const navigate = useNavigate();
  const [input, setInput] = useState("");
  const [autonomy, setAutonomy] = useState<Autonomy>("supervised");
  const [budgetUsd, setBudgetUsd] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const start = async (text: string) => {
    if (!text.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await createRun(settings, {
        input: text,
        autonomy,
        budget_usd: budgetUsd || undefined,
        conversational: true,
      });
      addRecentRun({ runId: res.run_id, input: text, createdAt: new Date().toISOString() });
      navigate(`/runs/${res.run_id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
      setSubmitting(false);
    }
  };

  return (
    <div className="chat-thread compose-only">
      <div className="compose-hero">
        <h1>Start a new chat</h1>
        {!isConfigured && <p className="callout warn">Set your identity in Settings above first.</p>}
        <div className="prompt-chips" role="group" aria-label="Suggested prompts">
          {suggestedPrompts.map((p) => (
            <button key={p.id} type="button" className="prompt-chip" title={p.hint} onClick={() => setInput(p.text)}>
              {p.label}
            </button>
          ))}
        </div>
      </div>

      {error && <p className="callout error">{error}</p>}

      <Composer
        value={input}
        onChange={setInput}
        onSubmit={() => start(input)}
        disabled={submitting || !isConfigured}
        placeholder="Describe the task for the agent to run…"
      >
        <details className="run-options">
          <summary>Run options</summary>
          <div className="run-options-fields">
            <label>
              Autonomy
              <select value={autonomy} onChange={(e) => setAutonomy(e.target.value as Autonomy)}>
                <option value="read_only">read_only</option>
                <option value="supervised">supervised</option>
                <option value="autonomous">autonomous</option>
              </select>
            </label>
            <label>
              Budget (USD)
              <input type="text" inputMode="decimal" placeholder="0.50" value={budgetUsd} onChange={(e) => setBudgetUsd(e.target.value)} />
            </label>
          </div>
        </details>
      </Composer>
    </div>
  );
}

/**
 * One backend session IS the conversation throughout its life --
 * kernel.RunConfig.Conversational's pause-not-terminate semantics mean a
 * plain reply suspends the session ("awaiting_input" status) rather than
 * ending it, and POST .../steer transparently resumes it in place
 * (internal/surfaces/rest/runctl.go's handleSteerRun). runId never changes
 * as the conversation grows, and there is no client-side chaining: the
 * live SSE subscription (useRunEvents) simply keeps delivering events
 * across turns for as long as the session stays open.
 */
function LiveThread({ runId, embedded }: { runId: string; embedded: boolean }) {
  const { settings, isConfigured } = useSettings();
  const navigate = useNavigate();

  const { events, live, state: connState, error: streamError, reconnect } = useRunEvents(settings, runId);
  const [run, setRun] = useState<GetRunResponse | null>(null);
  const [notFound, setNotFound] = useState(false);
  const [pendingApproval, setPendingApproval] = useState<ApprovalView | null>(null);
  const [composerInput, setComposerInput] = useState("");
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionBusy, setActionBusy] = useState(false);
  const listRef = useRef<HTMLDivElement>(null);

  const status = run?.status;
  const isTerminal = status ? TERMINAL_STATUSES.has(status.toLowerCase()) : false;
  const isSuspended = status === "suspended";
  const isAwaitingInput = status === "awaiting_input";

  const refreshRun = async () => {
    if (!isConfigured) return;
    try {
      const res = await getRun(settings, runId);
      setRun(res);
      setNotFound(false);
    } catch (err) {
      // A 404 means this id genuinely doesn't exist for this principal (a
      // stale bookmark, a run from a wiped dev database, ...) -- distinct
      // from a transient/backend-unreachable failure, where the status pill
      // should just stay stale and streamError already surfaces the problem.
      if (err instanceof ApiError && err.status === 404) setNotFound(true);
    }
  };

  useEffect(() => {
    setRun(null);
    setNotFound(false);
  }, [runId]);

  useEffect(() => {
    refreshRun();
    if (isTerminal || notFound) return;
    const t = setInterval(refreshRun, POLL_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, isConfigured, status, notFound]);

  useEffect(() => {
    if (events.some((e) => e.type === "terminal" || e.type === "awaiting_input")) refreshRun();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [events.length]);

  const refreshPendingApproval = async () => {
    try {
      const all = await listApprovals(settings);
      const mine = all.find((a) => a.session_id === runId && a.status === "pending");
      setPendingApproval(mine ?? null);
    } catch {
      // best-effort -- the tool card's own "awaiting approval" state still shows
    }
  };

  useEffect(() => {
    if (!isSuspended) {
      setPendingApproval(null);
      return;
    }
    refreshPendingApproval();
    const t = setInterval(refreshPendingApproval, POLL_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isSuspended, runId]);

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight, behavior: "smooth" });
  }, [events.length, pendingApproval, live]);

  const timeline = buildTimeline(events);
  const thinking = isThinking(status, timeline);
  const awaitingTool = findAwaitingApproval(timeline);

  const doSteer = async (text: string) => {
    if (!text.trim()) return;
    setActionBusy(true);
    setActionError(null);
    try {
      await steerRun(settings, runId, text);
      setComposerInput("");
      await refreshRun();
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setActionBusy(false);
    }
  };

  const doCancel = async () => {
    setActionBusy(true);
    setActionError(null);
    try {
      await cancelRun(settings, runId, "cancelled from web UI");
      await refreshRun();
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setActionBusy(false);
    }
  };

  const onApprovalResolved = async () => {
    setPendingApproval(null);
    await refreshRun();
    await refreshPendingApproval();
  };

  const isClarification = pendingApproval && extractToolId(pendingApproval) === ASK_CLARIFICATION_TOOL_ID;

  if (notFound) {
    return (
      <div className={`chat-thread not-found ${embedded ? "embedded" : ""}`}>
        <p className="hint">This chat could not be found — it may be from a different account, or the local dev database was reset.</p>
        {!embedded && (
          <button type="button" onClick={() => navigate("/")}>
            Start a new chat
          </button>
        )}
      </div>
    );
  }

  return (
    <div className={`chat-thread ${embedded ? "embedded" : ""}`}>
      {!embedded && (
        <div className="thread-status-bar">
          <span className={`status-pill status-${status ?? "unknown"}`}>{status ?? "loading…"}</span>
          <span className={`conn-pill conn-${connState}`}>live: {connState}</span>
          {connState === "error" && (
            <button type="button" className="secondary" onClick={reconnect}>
              Reconnect
            </button>
          )}
          {!isTerminal && (
            <button type="button" className="secondary danger-outline" disabled={actionBusy} onClick={doCancel}>
              Cancel
            </button>
          )}
        </div>
      )}
      {streamError && <p className="callout error">stream: {streamError}</p>}

      <div className="message-list" ref={listRef}>
        {timeline.map((item) => {
          if (item.kind === "tool") {
            if (item.childSessionId) return <SubAgentCard key={item.key} item={item as typeof item & { childSessionId: string }} />;
            return <ToolCallCard key={item.key} item={item} />;
          }
          return <MessageBubble key={item.key} item={item} />;
        })}

        {live?.kind === "content" && live.text && (
          // Draft: this turn's own live preview, not yet the durable
          // "content" event -- reset the instant that event lands
          // (useRunEvents' own doc comment on `live`), so this and its
          // confirmed MessageBubble counterpart never both show at once.
          <div className="msg assistant draft">
            <span className="msg-text" dangerouslySetInnerHTML={{ __html: renderMarkdown(live.text) }} />
          </div>
        )}

        {live?.kind === "tool_use" && (
          <div className="msg assistant thinking-row">
            <span className="thinking-label">Calling {live.toolName || "a tool"}</span>
          </div>
        )}

        {(live?.kind === "reasoning" || (!live && thinking)) && (
          <div className="msg assistant thinking-row">
            <span className="thinking-label">
              {awaitingTool ? `Using ${awaitingTool.toolId}` : "Thinking"}
            </span>
          </div>
        )}

        {isAwaitingInput && !pendingApproval && (
          <div className="msg assistant your-turn-row">
            <span className="your-turn-label">Your turn — send a message to continue.</span>
          </div>
        )}

        {pendingApproval &&
          (isClarification ? (
            <ClarificationCard approval={pendingApproval} onResolved={onApprovalResolved} />
          ) : (
            <ApprovalCard approval={pendingApproval} onResolved={onApprovalResolved} />
          ))}
      </div>

      {actionError && <p className="callout error">{actionError}</p>}

      {!embedded && (
        <ThreadComposer
          isSuspended={isSuspended}
          isTerminal={isTerminal}
          busy={actionBusy}
          value={composerInput}
          onChange={setComposerInput}
          onSubmit={() => doSteer(composerInput)}
        />
      )}
    </div>
  );
}

function ThreadComposer({
  isSuspended,
  isTerminal,
  busy,
  value,
  onChange,
  onSubmit,
}: {
  isSuspended: boolean;
  isTerminal: boolean;
  busy: boolean;
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
}) {
  if (isSuspended) {
    return (
      <div className="composer composer-disabled">
        <span className="hint">Waiting on the decision above.</span>
      </div>
    );
  }

  if (isTerminal) {
    return (
      <div className="composer composer-disabled">
        <span className="hint">This conversation has ended.</span>
      </div>
    );
  }

  return <Composer value={value} onChange={onChange} onSubmit={onSubmit} disabled={busy} placeholder="Send a message…" />;
}

function Composer({
  value,
  onChange,
  onSubmit,
  disabled,
  placeholder,
  children,
}: {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
  disabled?: boolean;
  placeholder?: string;
  children?: React.ReactNode;
}) {
  return (
    <form
      className="composer"
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit();
      }}
    >
      <textarea
        rows={2}
        value={value}
        placeholder={placeholder}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault();
            onSubmit();
          }
        }}
      />
      <button type="submit" disabled={disabled || !value.trim()}>
        Send
      </button>
      {children}
    </form>
  );
}

function extractToolId(approval: ApprovalView): string | undefined {
  if (typeof approval.context !== "object" || approval.context === null) return undefined;
  const v = (approval.context as Record<string, unknown>).tool_id;
  return typeof v === "string" ? v : undefined;
}
