import { useCallback, useEffect, useRef, useState } from "react";
import { eventsURL } from "./api";
import { readSSEStream, type ConnectionState } from "./sse";
import type { RunEvent, Settings } from "./types";

interface UseRunEventsResult {
  events: RunEvent[];
  state: ConnectionState;
  error: string | null;
  reconnect: () => void;
}

/**
 * Streams GET /v1/runs/{id}/events via fetch + manual SSE parsing (see
 * lib/sse.ts for why native EventSource can't be used here). The backend
 * replays full history on every subscribe, so a reconnect is safe -- events
 * are de-duplicated by event_id.
 */
export function useRunEvents(settings: Settings, runId: string | undefined): UseRunEventsResult {
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [state, setState] = useState<ConnectionState>("connecting");
  const [error, setError] = useState<string | null>(null);
  const [generation, setGeneration] = useState(0);
  const seenIds = useRef<Set<string>>(new Set());

  const reconnect = useCallback(() => {
    seenIds.current = new Set();
    setEvents([]);
    setError(null);
    setGeneration((g) => g + 1);
  }, []);

  useEffect(() => {
    if (!runId || !settings.baseUrl || !settings.tenantId || !settings.userId) {
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
            "X-Nexus-Tenant-ID": settings.tenantId,
            "X-Nexus-User-ID": settings.userId,
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
            try {
              const parsed = JSON.parse(frame.data) as RunEvent;
              if (seenIds.current.has(parsed.event_id)) return;
              seenIds.current.add(parsed.event_id);
              setEvents((prev) => [...prev, parsed]);
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
  }, [settings.baseUrl, settings.tenantId, settings.userId, runId, generation]);

  return { events, state, error, reconnect };
}
