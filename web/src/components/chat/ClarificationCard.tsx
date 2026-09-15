import { useState } from "react";
import { ApiError, denyApproval, grantApproval } from "../../lib/api";
import { useSettings } from "../../lib/settings";
import type { ApprovalView } from "../../lib/types";

interface ClarificationInput {
  question?: string;
  options?: string[];
}

/** Rendered instead of ApprovalCard when the pending approval's tool_id is
 * platform/ask_clarification (internal/tools/builtin/ask_clarification.go):
 * a question the model asked, answered by always granting with modified
 * input {"answer": ...} -- never a bare grant, which would re-execute with
 * the original {question, options} and no answer. Denying is "skip this
 * question" (the model sees a permission_denied outcome and continues, or
 * the run terminates, exactly like denying any other ask). */
export function ClarificationCard({ approval, onResolved }: { approval: ApprovalView; onResolved: () => void }) {
  const { settings } = useSettings();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [freeText, setFreeText] = useState("");

  const input = extractInput(approval.context);
  const question = input?.question ?? "The agent has a question.";
  const options = input?.options ?? [];

  const answer = async (value: string) => {
    if (!value.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await grantApproval(settings, approval.approval_id, { answer: value });
      onResolved();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const skip = async () => {
    setBusy(true);
    setError(null);
    try {
      await denyApproval(settings, approval.approval_id, "human skipped this question");
      onResolved();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="clarification">
      <div className="clarification-question">❓ {question}</div>

      {options.length > 0 ? (
        <div className="followups">
          {options.map((opt) => (
            <button key={opt} type="button" disabled={busy} onClick={() => answer(opt)}>
              {opt}
            </button>
          ))}
        </div>
      ) : (
        <div className="clarification-answer-row">
          <input
            type="text"
            placeholder="Type your answer…"
            value={freeText}
            onChange={(e) => setFreeText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") answer(freeText);
            }}
          />
          <button type="button" disabled={busy || !freeText.trim()} onClick={() => answer(freeText)}>
            Send
          </button>
        </div>
      )}

      <button type="button" className="secondary skip-question" disabled={busy} onClick={skip}>
        skip this question
      </button>

      {error && <p className="callout error">{error}</p>}
    </div>
  );
}

function extractInput(context: unknown): ClarificationInput | null {
  if (typeof context !== "object" || context === null) return null;
  const input = (context as Record<string, unknown>).input;
  if (typeof input !== "object" || input === null) return null;
  const obj = input as Record<string, unknown>;
  const options = Array.isArray(obj.options) ? obj.options.filter((o): o is string => typeof o === "string") : undefined;
  return { question: typeof obj.question === "string" ? obj.question : undefined, options };
}
