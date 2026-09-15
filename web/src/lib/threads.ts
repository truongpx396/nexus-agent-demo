// A "thread" is a client-side concept only -- the backend has none. Each
// backend run/session is one complete, independently-audited task from a
// single input to a terminal state (kernel/turns.go: a plain content reply
// with no tool call always terminates the run via TerminalCompleted --
// runctl.Steer's own doc comment confirms a session can't be steered once
// terminal). So "send another message" after a run finishes means starting
// a brand-new run, never resuming the old one.
//
// This file stitches a chain of those separate runs back into one visual
// conversation -- the same "client replays prior history on every call"
// pattern a stateless chat API's own client already uses. threadId is
// always the chain's FIRST run id (there is no separate id space); ChatPage
// still routes on /runs/:id using that same id, so the URL never changes as
// the conversation grows.

const STORAGE_KEY = "nexus-web-threads";

interface ThreadRecord {
  runIds: string[];
}

function loadAll(): Record<string, ThreadRecord> {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    return typeof parsed === "object" && parsed !== null ? parsed : {};
  } catch {
    return {};
  }
}

function saveAll(all: Record<string, ThreadRecord>): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(all));
  } catch {
    // best-effort; localStorage may be unavailable (private mode, quota)
  }
}

/** The full chain of run ids for threadId (oldest to newest, threadId
 * itself always first). A thread with no recorded follow-ups is just
 * [threadId] -- this never needs a separate "does a thread exist" check. */
export function getChain(threadId: string): string[] {
  const rec = loadAll()[threadId];
  return rec && rec.runIds.length > 0 ? rec.runIds : [threadId];
}

/** Records that newRunId continues threadId's conversation, and returns the
 * updated chain. */
export function appendToChain(threadId: string, newRunId: string): string[] {
  const all = loadAll();
  const existing = all[threadId]?.runIds ?? [threadId];
  const updated = [...existing, newRunId];
  all[threadId] = { runIds: updated };
  saveAll(all);
  return updated;
}

/** Every run id that is a FOLLOW-UP (not the anchor) of some known thread --
 * the sidebar drops these from GET /v1/sessions' own listing so one
 * conversation doesn't render as several separate rows. */
export function allFollowUpRunIds(): Set<string> {
  const all = loadAll();
  const ids = new Set<string>();
  for (const rec of Object.values(all)) {
    for (const id of rec.runIds.slice(1)) ids.add(id);
  }
  return ids;
}
