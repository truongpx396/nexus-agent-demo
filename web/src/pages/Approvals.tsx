import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, listApprovals } from "../lib/api";
import { useSettings } from "../lib/settings";
import type { ApprovalView } from "../lib/types";

const POLL_INTERVAL_MS = 5000;

export function Approvals() {
  const { settings, isConfigured } = useSettings();
  const [approvals, setApprovals] = useState<ApprovalView[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = async () => {
    if (!isConfigured) {
      setLoading(false);
      return;
    }
    try {
      const res = await listApprovals(settings);
      setApprovals(res ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, POLL_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [settings.baseUrl, settings.tenantId, settings.userId]);

  const pending = approvals.filter((a) => a.status === "pending");
  const rest = approvals.filter((a) => a.status !== "pending");

  return (
    <div className="page">
      <h1>Approvals</h1>
      {!isConfigured && (
        <p className="callout warn">Set your identity in Settings above to load approvals.</p>
      )}
      {error && <p className="callout error">{error}</p>}
      {loading && <p className="hint">Loading...</p>}

      {!loading && approvals.length === 0 && !error && (
        <p className="hint">No approvals for this tenant/user.</p>
      )}

      {pending.length > 0 && (
        <section>
          <h2>Pending ({pending.length})</h2>
          <ApprovalTable rows={pending} />
        </section>
      )}

      {rest.length > 0 && (
        <section>
          <h2>Other</h2>
          <ApprovalTable rows={rest} />
        </section>
      )}
    </div>
  );
}

function ApprovalTable({ rows }: { rows: ApprovalView[] }) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Status</th>
            <th>Tool</th>
            <th>Ask kind</th>
            <th>Expires</th>
            <th>Approval ID</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((a) => (
            <tr key={a.approval_id} className={a.status === "pending" ? "row-pending" : ""}>
              <td>
                <span className={`status-pill status-${a.status}`}>{a.status}</span>
              </td>
              <td>{a.tool_id}</td>
              <td>{a.ask_kind}</td>
              <td>{formatTime(a.expires_at)}</td>
              <td>
                <Link to={`/approvals/${a.approval_id}`} className="mono">
                  {a.approval_id}
                </Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}
