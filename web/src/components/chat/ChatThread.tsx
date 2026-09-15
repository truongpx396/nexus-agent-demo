import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ApiError, cancelRun, createRun, fetchRunHistory, getRun, listApprovals, steerRun } from "../../lib/api";
import { addRecentRun } from "../../lib/recentRuns";
import { useSettings } from "../../lib/settings";
import { suggestedPrompts } from "../../lib/suggestedPrompts";
import { appendToChain, getChain } from "../../lib/threads";
import { buildTimeline, findAwaitingApproval, isThinking, type TimelineItem } from "../../lib/timeline";
import type { Autonomy, ApprovalView, GetRunResponse, RunEvent } from "../../lib/types";
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
  return <LiveThread threadId={runId} embedded={Boolean(embedded)} />;
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
      const res = await createRun(settings, { input: text, autonomy, budget_usd: budgetUsd || undefined });
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
 * A visual conversation is a client-side CHAIN of backend runs
 * (lib/threads.ts) -- the backend itself has no multi-turn session; a run
 * is one complete task from a single input to a terminal state, and
 * runctl.Steer refuses once it's terminal. threadId (the URL's :id) is
 * always the chain's first run id and never changes as the conversation
 * grows; `chain`'s LAST id is the one actually live/actionable right now.
 * Earlier runs in the chain are fetched once (fetchRunHistory) and their
 * events prepended to the live tail's own timeline, so the whole thing
 * reads as one continuous scroll.
 */
function LiveThread({ threadId, embedded }: { threadId: string; embedded: boolean }) {
  const { settings, isConfigured } = useSettings();
  const navigate = useNavigate();
  const [chain, setChain] = useState<string[]>(() => getChain(threadId));
  const [priorEventsByRun, setPriorEventsByRun] = useState<Record<string, RunEvent[]>>({});
  const requestedRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    setChain(getChain(threadId));
    setPriorEventsByRun({});
    requestedRef.current = new Set();
  }, [threadId]);

  const latestRunId = chain[chain.length - 1];
  const priorRunIds = chain.slice(0, -1);

  useEffect(() => {
    const missing = priorRunIds.filter((id) => !requestedRef.current.has(id));
    if (missing.length === 0) return;
    for (const id of missing) requestedRef.current.add(id);
    (async () => {
      for (const id of missing) {
        try {
          const hist = await fetchRunHistory(settings, id);
          setPriorEventsByRun((prev) => ({ ...prev, [id]: hist }));
        } catch {
          // best-effort -- that turn's history is just missing from the replay, not a hard failure
          setPriorEventsByRun((prev) => ({ ...prev, [id]: [] }));
        }
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chain, settings.baseUrl, settings.token]);

  const { events: liveEvents, state: connState, error: streamError, reconnect } = useRunEvents(settings, latestRunId);
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

  const refreshRun = async () => {
    if (!isConfigured) return;
    try {
      const res = await getRun(settings, latestRunId);
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
    refreshRun();
    if (isTerminal || notFound) return;
    const t = setInterval(refreshRun, POLL_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [latestRunId, isConfigured, status, notFound]);

  useEffect(() => {
    if (liveEvents.some((e) => e.type === "terminal")) refreshRun();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [liveEvents.length]);

  const refreshPendingApproval = async () => {
    try {
      const all = await listApprovals(settings);
      const mine = all.find((a) => a.session_id === latestRunId && a.status === "pending");
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
  }, [isSuspended, latestRunId]);

  const allEvents = [...priorRunIds.flatMap((id) => priorEventsByRun[id] ?? []), ...liveEvents];

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight, behavior: "smooth" });
  }, [allEvents.length, pendingApproval]);

  const timeline = buildTimeline(allEvents);
  const thinking = isThinking(status, timeline);
  const awaitingTool = findAwaitingApproval(timeline);

  const doSteer = async (text: string) => {
    if (!text.trim()) return;
    setActionBusy(true);
    setActionError(null);
    try {
      await steerRun(settings, latestRunId, text);
      setComposerInput("");
      await refreshRun();
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setActionBusy(false);
    }
  };

  // sendFollowUp is what keeps the conversation going once the latest run
  // has already finished: the backend has no way to resume a terminal
  // session (this file's own doc comment), so this starts a genuinely new
  // run, replaying the visible user/assistant turns so far as its own
  // input -- the same "client replays prior history" shape a stateless
  // chat API's client already uses -- and extends the chain rather than
  // navigating anywhere, so the URL and the on-screen scroll both stay put.
  const sendFollowUp = async (text: string) => {
    if (!text.trim()) return;
    setActionBusy(true);
    setActionError(null);
    try {
      const transcript = timeline
        .filter((i): i is Extract<TimelineItem, { kind: "user" | "assistant" }> => i.kind === "user" || i.kind === "assistant")
        .map((i) => `${i.kind === "user" ? "User" : "Assistant"}: ${i.text}`)
        .join("\n\n");
      const combinedInput = transcript ? `${transcript}\n\nUser: ${text}` : text;
      const res = await createRun(settings, { input: combinedInput, autonomy: "supervised" });
      setChain(appendToChain(threadId, res.run_id));
      setComposerInput("");
      setRun(null);
      setNotFound(false);
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
      await cancelRun(settings, latestRunId, "cancelled from web UI");
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

        {thinking && (
          <div className="msg assistant thinking-row">
            <span className="thinking-label">
              {awaitingTool ? `Using ${awaitingTool.toolId}` : "Thinking"}
            </span>
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
          busy={actionBusy}
          value={composerInput}
          onChange={setComposerInput}
          onSubmit={() => (isTerminal ? sendFollowUp(composerInput) : doSteer(composerInput))}
        />
      )}
    </div>
  );
}

function ThreadComposer({
  isSuspended,
  busy,
  value,
  onChange,
  onSubmit,
}: {
  isSuspended: boolean;
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
