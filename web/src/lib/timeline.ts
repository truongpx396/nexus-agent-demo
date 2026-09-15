// Folds the flat, durable RunEvent[] (already delivered in seq order by
// useRunEvents) into the discriminated-union list ChatThread renders
// directly. This is the one piece of real translation logic between the
// backend's event log and a chat UI -- everything else is presentation.
//
// Pairing rules below are re-derived from the backend, not guessed:
//   - tool_result.pair_ref is the pairing tool_use event's event_id
//     (kernel/events.go's appendToolResult).
//   - approval_requested and delegation_requested carry NO pair_ref
//     (kernel/terminal.go's suspendForApproval/suspendForDelegation both
//     pass pairRef=nil) -- they're appended immediately after their own
//     tool_use with nothing in between (the run suspends right there), so
//     "the most recent still-open tool_use with a matching tool_id" is a
//     reliable pairing for them.
//   - EventThought is never sent with a body at all (rest/run_events.go's
//     toEventDTO), so there is nothing to render for it -- "thinking" is
//     inferred separately, from run/tool state, not from this list.
import type { RunEvent } from "./types";

export type TimelineItem =
  | { key: string; kind: "user"; text: string; ts: string }
  | { key: string; kind: "assistant"; text: string; ts: string }
  | {
      key: string;
      kind: "tool";
      toolUseEventId: string;
      toolId: string;
      input: unknown;
      result?: { output: unknown; isError: boolean; reason?: string };
      childSessionId?: string;
      awaitingApproval: boolean;
      ts: string;
    }
  | { key: string; kind: "system"; text: string; tone: "info" | "warn" | "error"; ts: string };

function asRecord(v: unknown): Record<string, unknown> {
  return typeof v === "object" && v !== null ? (v as Record<string, unknown>) : {};
}

function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

const TERMINAL_REASON_TEXT: Record<string, string> = {
  completed: "Done.",
  max_turns_exceeded: "Stopped — hit the maximum number of turns for this run.",
  cost_exhausted: "Stopped — the cost ceiling for this run was reached.",
  aborted: "Cancelled.",
  stuck_terminated: "Stopped — the agent looked like it was stuck in a repeating loop.",
  permission_denied: "Stopped — a required permission was denied.",
  context_overflow: "Stopped — the conversation grew past the model's context window.",
  error: "Stopped — an error occurred.",
  refused: "The model declined to continue with this request.",
};

type SystemTone = "info" | "warn" | "error";

const TERMINAL_REASON_TONE: Record<string, SystemTone> = {
  completed: "info",
  aborted: "info",
  refused: "warn",
  max_turns_exceeded: "warn",
  cost_exhausted: "warn",
  stuck_terminated: "warn",
  permission_denied: "warn",
  context_overflow: "warn",
  error: "error",
};

function terminalNote(reason: string | undefined, detail: string | undefined, ts: string, eventId: string): TimelineItem {
  const base = reason ? TERMINAL_REASON_TEXT[reason] ?? `Stopped (${reason}).` : "Stopped.";
  const tone = (reason && TERMINAL_REASON_TONE[reason]) || "info";
  const text = detail && reason !== "completed" ? `${base} ${detail}` : base;
  return { key: eventId, kind: "system", text, tone, ts };
}

export function buildTimeline(events: RunEvent[]): TimelineItem[] {
  const items: TimelineItem[] = [];
  const indexByToolUseEventId = new Map<string, number>();
  const openToolUseEventIdByToolId = new Map<string, string>();

  const closeOpenTool = (toolId: string, toolUseEventId: string) => {
    if (openToolUseEventIdByToolId.get(toolId) === toolUseEventId) {
      openToolUseEventIdByToolId.delete(toolId);
    }
  };

  for (const ev of events) {
    const body = asRecord(ev.body);
    switch (ev.type) {
      case "user_message":
        items.push({ key: ev.event_id, kind: "user", text: str(body.body) ?? "", ts: ev.created_at });
        break;

      case "content":
        items.push({ key: ev.event_id, kind: "assistant", text: str(body.body) ?? "", ts: ev.created_at });
        break;

      case "tool_use": {
        const toolId = ev.tool_id ?? str(body.tool_name) ?? "unknown_tool";
        const idx = items.length;
        items.push({
          key: ev.event_id, kind: "tool", toolUseEventId: ev.event_id, toolId,
          input: body.input, awaitingApproval: false, ts: ev.created_at,
        });
        indexByToolUseEventId.set(ev.event_id, idx);
        openToolUseEventIdByToolId.set(toolId, ev.event_id);
        break;
      }

      case "tool_result": {
        const pairId = ev.pair_ref;
        const idx = pairId ? indexByToolUseEventId.get(pairId) : undefined;
        const item = idx !== undefined ? items[idx] : undefined;
        if (item?.kind === "tool") {
          item.result = { output: body.output, isError: Boolean(body.is_error), reason: str(body.reason) };
          item.awaitingApproval = false;
          if (pairId) closeOpenTool(item.toolId, pairId);
        }
        break;
      }

      case "approval_requested": {
        const toolId = str(body.tool_id) ?? ev.tool_id ?? "";
        const openId = openToolUseEventIdByToolId.get(toolId);
        const idx = openId ? indexByToolUseEventId.get(openId) : undefined;
        const item = idx !== undefined ? items[idx] : undefined;
        if (item?.kind === "tool") item.awaitingApproval = true;
        break;
      }

      case "delegation_requested": {
        const toolId = str(body.tool_id) ?? ev.tool_id ?? "";
        const childSessionId = str(body.child_session_id);
        const openId = openToolUseEventIdByToolId.get(toolId);
        const idx = openId ? indexByToolUseEventId.get(openId) : undefined;
        const item = idx !== undefined ? items[idx] : undefined;
        if (item?.kind === "tool" && childSessionId) item.childSessionId = childSessionId;
        break;
      }

      case "terminal":
        items.push(terminalNote(str(body.reason), str(body.detail), ev.created_at, ev.event_id));
        break;

      case "stuck_suspected":
        items.push({
          key: ev.event_id, kind: "system", tone: "warn", ts: ev.created_at,
          text: `Possible repeating loop detected (${str(body.reason) ?? "unrecognized pattern"}).`,
        });
        break;

      case "condensation":
        items.push({
          key: ev.event_id, kind: "system", tone: "info", ts: ev.created_at,
          text: "Earlier context was summarized to stay within the model's context window.",
        });
        break;

      case "approval_granted":
      case "approval_granted_modified":
        items.push({ key: ev.event_id, kind: "system", tone: "info", ts: ev.created_at, text: "Approved." });
        break;

      case "approval_denied":
        items.push({ key: ev.event_id, kind: "system", tone: "warn", ts: ev.created_at, text: "Denied." });
        break;

      case "approval_expired":
        items.push({ key: ev.event_id, kind: "system", tone: "warn", ts: ev.created_at, text: "The approval request expired before anyone answered it." });
        break;

      case "approval_invalidated":
      case "approval_resolution_refused":
      case "approval_mismatch":
        items.push({ key: ev.event_id, kind: "system", tone: "warn", ts: ev.created_at, text: "That approval could not be resolved as requested." });
        break;

      case "autonomy_tightened":
        items.push({
          key: ev.event_id, kind: "system", tone: "info", ts: ev.created_at,
          text: `Autonomy tightened to "${str(body.to) ?? "a stricter level"}".`,
        });
        break;

      default:
        // thought (never has a body -- redacted, not just skipped here),
        // budget_decision, context_pruned, tool_loaded, memory_loaded, and
        // every other structural/audit-only event type: not chat-relevant.
        break;
    }
  }
  return items;
}

/** Whether a "Thinking…" indicator belongs at the bottom of the thread right
 * now -- derived, never from literal reasoning content (never sent to the
 * client at all; see this file's own header comment). */
export function isThinking(status: string | undefined, items: TimelineItem[]): boolean {
  if (status !== "running" && status !== "queued") return false;
  const last = items[items.length - 1];
  // A pending tool item already shows its own "Using {tool}" state (or the
  // approval card, if it's awaiting one) -- "Thinking" only covers the gap
  // before the model has said or done anything for its current turn yet.
  if (last?.kind === "tool" && !last.result) return false;
  return true;
}

/** The tool item, if any, currently awaiting a human decision -- there is at
 * most one, since a run suspends the moment one tool_use resolves Ask. */
export function findAwaitingApproval(items: TimelineItem[]): Extract<TimelineItem, { kind: "tool" }> | undefined {
  for (let i = items.length - 1; i >= 0; i--) {
    const item = items[i];
    if (item.kind === "tool" && item.awaitingApproval) return item;
  }
  return undefined;
}
