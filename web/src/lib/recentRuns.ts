// The REST API has no "list runs" endpoint (only GET /v1/runs/{id}), so
// this is a purely client-side convenience: remember run ids this browser
// created/visited, to make the app navigable after a refresh. It is not
// backend state and never claims to be.

const STORAGE_KEY = "nexus-web-recent-runs";
const MAX_ENTRIES = 20;

export interface RecentRun {
  runId: string;
  input: string;
  createdAt: string;
}

export function loadRecentRuns(): RecentRun[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

export function addRecentRun(entry: RecentRun): void {
  const existing = loadRecentRuns().filter((r) => r.runId !== entry.runId);
  const next = [entry, ...existing].slice(0, MAX_ENTRIES);
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  } catch {
    // best-effort; localStorage may be unavailable (private mode, quota)
  }
}
