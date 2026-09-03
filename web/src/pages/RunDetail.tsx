import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { ApiError, cancelRun, getRun, steerRun, tightenAutonomy } from "../lib/api";
import { useSettings } from "../lib/settings";
import { useRunEvents } from "../lib/useRunEvents";
import type { Autonomy, GetRunResponse } from "../lib/types";
import { JsonView } from "../components/JsonView";
import { TaintBadges } from "../components/TaintBadges";

const POLL_INTERVAL_MS = 3000;
const TERMINAL_STATUSES = new Set(["completed", "failed", "cancelled", "canceled", "aborted"]);

export function RunDetail() {
  const { id } = useParams<{ id: string }>();
  const { settings, isConfigured } = useSettings();
  const { events, state, error: streamError, reconnect } = useRunEvents(settings, id);

  const [run, setRun] = useState<GetRunResponse | null>(null);
  const [runError, setRunError] = useState<string | null>(null);

  const [actionError, setActionError] = useState<string | null>(null);
  const [actionBusy, setActionBusy] = useState(false);
  const [steerInput, setSteerInput] = useState("");
  const [autonomyTarget, setAutonomyTarget] = useState<Autonomy>("read_only");
  const [cancelReason, setCancelReason] = useState("run cancelled from web UI");

  const logRef = useRef<HTMLDivElement>(null);

  const refreshRun = async () => {
    if (!id || !isConfigured) return;
    try {
      const res = await getRun(settings, id);
      setRun(res);
      setRunError(null);
    } catch (err) {
      setRunError(err instanceof ApiError ? err.message : String(err));
    }
  };

  useEffect(() => {
    refreshRun();
    const isTerminal = run && TERMINAL_STATUSES.has(run.status.toLowerCase());
    if (isTerminal) return;
    const t = setInterval(refreshRun, POLL_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, isConfigured, run?.status]);

  // Re-poll status immediately when a `terminal` SSE frame arrives, rather
  // than waiting for the next poll tick.
  useEffect(() => {
    if (events.some((e) => e.type === "terminal")) refreshRun();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [events.length]);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
  }, [events.length]);

  const runAction = async (fn: () => Promise<unknown>) => {
    setActionBusy(true);
    setActionError(null);
    try {
      await fn();
      await refreshRun();
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setActionBusy(false);
    }
  };

  if (!id) return <p className="callout error">No run id in URL.</p>;

  const isTerminal = run ? TERMINAL_STATUSES.has(run.status.toLowerCase()) : false;

  return (
    <div className="page">
      <h1>
        Run <span className="mono">{id}</span>
      </h1>

      {!isConfigured && (
        <p className="callout warn">Set your identity in Settings above to load this run.</p>
      )}

      <section className="run-status-bar">
        <span className={`status-pill status-${run?.status ?? "unknown"}`}>
          {run?.status ?? "loading..."}
        </span>
        <span className={`conn-pill conn-${state}`}>events: {state}</span>
        {state === "error" && (
          <button type="button" onClick={reconnect}>
            Reconnect
          </button>
        )}
      </section>

      {runError && <p className="callout error">{runError}</p>}
      {streamError && <p className="callout error">stream: {streamError}</p>}

      {run?.terminal_reason && (
        <div className="terminal-reason">
          <strong>Terminal reason:</strong> {run.terminal_reason}
        </div>
      )}

      <section className="run-controls">
        <h2>Controls</h2>
        <div className="controls-row">
          <button
            type="button"
            disabled={actionBusy || isTerminal}
            onClick={() => runAction(() => cancelRun(settings, id, cancelReason))}
          >
            Cancel
          </button>
          <input
            type="text"
            value={cancelReason}
            onChange={(e) => setCancelReason(e.target.value)}
            placeholder="cancel reason"
          />
        </div>

        <div className="controls-row">
          <button
            type="button"
            disabled={actionBusy || isTerminal || !steerInput.trim()}
            onClick={() => runAction(() => steerRun(settings, id, steerInput)).then(() => setSteerInput(""))}
          >
            Steer
          </button>
          <input
            type="text"
            value={steerInput}
            onChange={(e) => setSteerInput(e.target.value)}
            placeholder="new input to inject"
          />
        </div>

        <div className="controls-row">
          <button
            type="button"
            disabled={actionBusy || isTerminal}
            onClick={() => runAction(() => tightenAutonomy(settings, id, autonomyTarget))}
          >
            Tighten autonomy
          </button>
          <select value={autonomyTarget} onChange={(e) => setAutonomyTarget(e.target.value as Autonomy)}>
            <option value="read_only">read_only</option>
            <option value="supervised">supervised</option>
            <option value="autonomous">autonomous</option>
          </select>
          <span className="hint">tightening only — the backend refuses widening</span>
        </div>

        {actionError && <p className="callout error">{actionError}</p>}
      </section>

      <section className="event-log-section">
        <h2>
          Event log <span className="hint">({events.length} events, newest at bottom)</span>
        </h2>
        <div className="event-log" ref={logRef}>
          {events.length === 0 && <p className="hint">No events yet.</p>}
          {events.map((ev) => (
            <div key={ev.event_id} className={`event-row event-actor-${ev.actor}`}>
              <div className="event-row-head">
                <span className="event-type">{ev.type}</span>
                <span className="event-actor">{ev.actor}</span>
                {ev.tool_id && <span className="event-tool">tool: {ev.tool_id}</span>}
                {ev.model_id && <span className="event-model">model: {ev.model_id}</span>}
                <span className="event-seq">#{ev.seq}</span>
                <span className="event-time">{formatTime(ev.created_at)}</span>
              </div>
              <TaintBadges body={ev.body} />
              {ev.body !== undefined && <JsonView value={ev.body} />}
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString();
  } catch {
    return iso;
  }
}
