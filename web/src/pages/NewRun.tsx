import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { createRun, ApiError } from "../lib/api";
import { addRecentRun, loadRecentRuns } from "../lib/recentRuns";
import { useSettings } from "../lib/settings";
import type { Autonomy } from "../lib/types";

export function NewRun() {
  const { settings, isConfigured } = useSettings();
  const navigate = useNavigate();

  const [input, setInput] = useState("");
  const [dataLabel, setDataLabel] = useState("");
  const [difficulty, setDifficulty] = useState("");
  const [autonomy, setAutonomy] = useState<Autonomy>("supervised");
  const [budgetUsd, setBudgetUsd] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [jumpId, setJumpId] = useState("");
  const recent = loadRecentRuns();

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!input.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await createRun(settings, {
        input,
        data_label: dataLabel || undefined,
        difficulty: difficulty || undefined,
        autonomy,
        budget_usd: budgetUsd || undefined,
      });
      addRecentRun({ runId: res.run_id, input, createdAt: new Date().toISOString() });
      navigate(`/runs/${res.run_id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="page">
      <h1>Start a run</h1>
      {!isConfigured && (
        <p className="callout warn">
          Set your Base URL, Tenant ID, and User ID in Settings above before starting a run.
        </p>
      )}

      <form className="form" onSubmit={onSubmit}>
        <label>
          Input <span className="required">*</span>
          <textarea
            required
            rows={5}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Describe the task for the agent to run..."
          />
        </label>

        <div className="form-row">
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
            <input
              type="text"
              inputMode="decimal"
              placeholder="0.50"
              value={budgetUsd}
              onChange={(e) => setBudgetUsd(e.target.value)}
            />
          </label>
        </div>

        <div className="form-row">
          <label>
            Data label <span className="optional">(optional)</span>
            <input
              type="text"
              list="data-label-options"
              placeholder="internal"
              value={dataLabel}
              onChange={(e) => setDataLabel(e.target.value)}
            />
            <datalist id="data-label-options">
              <option value="internal" />
              <option value="confidential" />
              <option value="public" />
            </datalist>
          </label>

          <label>
            Difficulty <span className="optional">(optional)</span>
            <input
              type="text"
              list="difficulty-options"
              placeholder="simple"
              value={difficulty}
              onChange={(e) => setDifficulty(e.target.value)}
            />
            <datalist id="difficulty-options">
              <option value="simple" />
              <option value="complex" />
            </datalist>
          </label>
        </div>

        {error && <p className="callout error">{error}</p>}

        <button type="submit" disabled={submitting || !input.trim()}>
          {submitting ? "Starting..." : "Start run"}
        </button>
      </form>

      <div className="jump-to-run">
        <h2>Jump to a run</h2>
        <form
          className="form-inline"
          onSubmit={(e) => {
            e.preventDefault();
            if (jumpId.trim()) navigate(`/runs/${jumpId.trim()}`);
          }}
        >
          <input
            type="text"
            placeholder="run id (uuid)"
            value={jumpId}
            onChange={(e) => setJumpId(e.target.value)}
          />
          <button type="submit" disabled={!jumpId.trim()}>
            View
          </button>
        </form>
      </div>

      {recent.length > 0 && (
        <div className="recent-runs">
          <h2>Recent runs (this browser)</h2>
          <ul>
            {recent.map((r) => (
              <li key={r.runId}>
                <a href={`/runs/${r.runId}`} onClick={(e) => { e.preventDefault(); navigate(`/runs/${r.runId}`); }}>
                  {r.runId}
                </a>
                <span className="recent-run-input">{r.input}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}
