import { useEffect, useState } from "react";
import { NavLink, useNavigate } from "react-router-dom";
import { listSessions } from "../lib/api";
import { loadRecentRuns } from "../lib/recentRuns";
import { useSettings } from "../lib/settings";
import type { SessionSummary } from "../lib/types";

const POLL_INTERVAL_MS = 8000;

interface Thread {
  runId: string;
  status: string;
  createdAt: string;
  preview?: string;
}

/** Session list -- GET /v1/sessions (backend, authoritative id/status/
 * timestamp) left-joined with the browser-local recentRuns cache (for the
 * input-text preview, when this browser is what started the thread; a
 * session opened on another device just shows its id). */
export function Sidebar() {
  const { settings, isConfigured } = useSettings();
  const navigate = useNavigate();
  const [remote, setRemote] = useState<SessionSummary[] | null>(null);

  const refresh = async () => {
    if (!isConfigured) return;
    try {
      setRemote(await listSessions(settings));
    } catch {
      // best-effort -- fall back to the local cache below
    }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, POLL_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [settings.baseUrl, settings.token]);

  const threads = mergeThreads(remote, loadRecentRuns());

  return (
    <aside className="sidebar">
      <button type="button" className="new-chat-button" onClick={() => navigate("/")}>
        + New chat
      </button>

      <nav className="sidebar-nav">
        <NavLink to="/approvals">Approvals</NavLink>
      </nav>

      <div className="sidebar-section-label">Chats</div>
      <div className="thread-list">
        {threads.length === 0 && <p className="hint">No chats yet.</p>}
        {threads.map((t) => (
          <NavLink key={t.runId} to={`/runs/${t.runId}`} className="thread-item">
            <span className={`status-dot status-${t.status}`} />
            <span className="thread-item-text">
              <span className="thread-item-preview">{t.preview ?? t.runId}</span>
              <span className="thread-item-time">{formatTime(t.createdAt)}</span>
            </span>
          </NavLink>
        ))}
      </div>
    </aside>
  );
}

// mergeThreads maps GET /v1/sessions' own rows 1:1 to sidebar rows -- one
// conversation is one backend session throughout its life (kernel.RunConfig.
// Conversational's pause-not-terminate semantics), so there is no client-side
// chain to collapse any more.
function mergeThreads(remote: SessionSummary[] | null, local: ReturnType<typeof loadRecentRuns>): Thread[] {
  const previewById = new Map(local.map((r) => [r.runId, r.input]));

  if (remote) {
    return remote
      .map((s) => ({
        runId: s.run_id,
        status: s.status,
        createdAt: s.created_at,
        preview: previewById.get(s.run_id),
      }))
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
  }

  // Backend unreachable -- fall back to whatever this browser remembers.
  return local.map((r) => ({ runId: r.runId, status: "unknown", createdAt: r.createdAt, preview: r.input }));
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}
