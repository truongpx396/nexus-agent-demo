import { useCallback, useEffect, useRef, useState } from "react";
import { eventsURL } from "./api";
import { readSSEStream, type ConnectionState } from "./sse";
import type { DeltaFrame, RunEvent, Settings } from "./types";

/** Live, best-effort preview of the turn currently in flight -- built from
 * `event: delta` SSE frames (DeltaFrame), never from `events`. Reset to null
 * the moment the next durable event lands (that event is the authoritative
 * version of whatever this was previewing) or on reconnect. There is no
 * resume/replay for this: a client that wasn't subscribed for a given delta
 * simply never sees it, same as the backend's own DeltaDTO doc comment. */
export type LiveTurnState =
  | { kind: "content"; text: string }
  | { kind: "tool_use"; toolUseId: string; toolName: string }
  | { kind: "reasoning" };

interface UseRunEventsResult {
  events: RunEvent[];
  live: LiveTurnState | null;
  state: ConnectionState;
  error: string | null;
  reconnect: () => void;
}

/**
 * Streams GET /v1/runs/{id}/events via fetch + manual SSE parsing (see
 * lib/sse.ts for why native EventSource can't be used here). The backend
 * replays full history on every subscribe, so a reconnect is safe -- events
 * are de-duplicated by event_id. `delta` frames are a separate, never-
 * replayed stream folded into `live` instead -- see LiveTurnState above.
 */
export function useRunEvents(settings: Settings, runId: string | undefined): UseRunEventsResult {
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [live, setLive] = useState<LiveTurnState | null>(null);
  const [state, setState] = useState<ConnectionState>("connecting");
  const [error, setError] = useState<string | null>(null);
  const [generation, setGeneration] = useState(0);
  const seenIds = useRef<Set<string>>(new Set());

  const reconnect = useCallback(() => {
    seenIds.current = new Set();
    setEvents([]);
    setLive(null);
    setError(null);
    setGeneration((g) => g + 1);
  }, []);

  useEffect(() => {
    if (!runId || !settings.baseUrl || !settings.token) {
      setState("closed");
      return;
    }

    const controller = new AbortController();
    setState("connecting");
    setError(null);

    (async () => {
      try {
        const res = await fetch(eventsURL(settings, runId), {
          headers: {
            Authorization: `Bearer ${settings.token}`,
          },
          signal: controller.signal,
        });
        if (!res.ok) {
          const text = await res.text().catch(() => res.statusText);
          throw new Error(text || `HTTP ${res.status}`);
        }
        setState("open");
        await readSSEStream(
          res,
          (frame) => {
            if (frame.event === "error") {
              try {
                const parsed = JSON.parse(frame.data) as { error?: string };
                setError(parsed.error ?? frame.data);
              } catch {
                setError(frame.data);
              }
              return;
            }
            if (frame.event === "delta") {
              try {
                const d = JSON.parse(frame.data) as DeltaFrame;
                setLive((prev) => {
                  if (d.kind === "content") {
                    const already = prev?.kind === "content" ? prev.text : "";
                    return { kind: "content", text: already + (d.text ?? "") };
                  }
                  if (d.kind === "tool_use") {
                    return { kind: "tool_use", toolUseId: d.tool_use_id ?? "", toolName: d.tool_name ?? "" };
                  }
                  return { kind: "reasoning" };
                });
              } catch {
                // Best-effort preview -- an unparseable delta frame just
                // means one less live update, never surfaced as an error
                // the way an unparseable durable event is below.
              }
              return;
            }
            try {
              const parsed = JSON.parse(frame.data) as RunEvent;
              if (seenIds.current.has(parsed.event_id)) return;
              seenIds.current.add(parsed.event_id);
              setEvents((prev) => [...prev, parsed]);
              // The durable event this turn's deltas were previewing (or
              // its successor, if this turn produced none) has now landed
              // -- whatever the live preview was showing is either about to
              // be rendered from `events` instead, or stale; either way it
              // must not keep showing next to its own confirmed version.
              setLive(null);
            } catch {
              // Not JSON we recognize -- surface as an error line rather
              // than silently dropping it.
              setError(`unparseable event frame (type=${frame.event})`);
            }
          },
          controller.signal,
        );
        if (!controller.signal.aborted) setState("closed");
      } catch (err) {
        if (controller.signal.aborted) return;
        setError((err as Error).message);
        setState("error");
      }
    })();

    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [settings.baseUrl, settings.token, runId, generation]);

  return { events, live, state, error, reconnect };
}
