// Thin REST client for internal/surfaces/rest. Every request attaches the
// two dev-auth headers the backend reads fresh per-request
// (X-Nexus-Tenant-ID / X-Nexus-User-ID) -- no cookies, no bearer tokens.
//
// Every non-2xx response is a plain-text body (Go's http.Error), never a
// JSON error envelope -- ApiError below carries that text verbatim.
import type {
  ApprovalView,
  CreateRunRequest,
  CreateRunResponse,
  ForkView,
  GetRunResponse,
  ResumeOutcome,
  Settings,
} from "./types";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message || `HTTP ${status}`);
    this.status = status;
    this.name = "ApiError";
  }
}

function authHeaders(s: Settings): Record<string, string> {
  return {
    "X-Nexus-Tenant-ID": s.tenantId,
    "X-Nexus-User-ID": s.userId,
  };
}

async function request<T>(s: Settings, path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`${s.baseUrl}${path}`, {
      ...init,
      headers: {
        ...authHeaders(s),
        ...(init?.body ? { "content-type": "application/json" } : {}),
        ...(init?.headers ?? {}),
      },
    });
  } catch (err) {
    // Most commonly a CORS failure or the backend not running -- see
    // web/README.md for why this repo doesn't (and can't, from here) fix
    // that on the Go side.
    throw new ApiError(0, `network error contacting ${s.baseUrl}: ${(err as Error).message}`);
  }
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new ApiError(res.status, text || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

export function createRun(s: Settings, body: CreateRunRequest): Promise<CreateRunResponse> {
  const payload: CreateRunRequest = { input: body.input };
  if (body.data_label) payload.data_label = body.data_label;
  if (body.difficulty) payload.difficulty = body.difficulty;
  if (body.autonomy) payload.autonomy = body.autonomy;
  if (body.budget_usd) payload.budget_usd = body.budget_usd;
  return request<CreateRunResponse>(s, "/v1/runs", {
    method: "POST",
    body: JSON.stringify(payload),
  });
}

export function getRun(s: Settings, id: string): Promise<GetRunResponse> {
  return request<GetRunResponse>(s, `/v1/runs/${id}`);
}

export function cancelRun(s: Settings, id: string, reason: string): Promise<unknown> {
  return request(s, `/v1/runs/${id}/cancel`, {
    method: "POST",
    body: JSON.stringify({ reason }),
  });
}

export function steerRun(s: Settings, id: string, input: string): Promise<unknown> {
  return request(s, `/v1/runs/${id}/steer`, {
    method: "POST",
    body: JSON.stringify({ input }),
  });
}

export function tightenAutonomy(s: Settings, id: string, target: string): Promise<unknown> {
  return request(s, `/v1/runs/${id}/autonomy`, {
    method: "POST",
    body: JSON.stringify({ target }),
  });
}

export function forkRun(
  s: Settings,
  id: string,
  atSeq: number,
  model?: string,
): Promise<ForkView> {
  const payload: { at_seq: number; model?: string } = { at_seq: atSeq };
  if (model) payload.model = model;
  return request<ForkView>(s, `/v1/runs/${id}/fork`, {
    method: "POST",
    body: JSON.stringify(payload),
  });
}

export function listApprovals(s: Settings): Promise<ApprovalView[]> {
  return request<ApprovalView[]>(s, "/v1/approvals");
}

export function getApproval(s: Settings, id: string): Promise<ApprovalView> {
  return request<ApprovalView>(s, `/v1/approvals/${id}`);
}

export function grantApproval(
  s: Settings,
  id: string,
  modifiedInput?: unknown,
): Promise<ResumeOutcome> {
  const body = modifiedInput !== undefined ? JSON.stringify({ modified_input: modifiedInput }) : undefined;
  return request<ResumeOutcome>(s, `/v1/approvals/${id}/grant`, {
    method: "POST",
    body,
  });
}

export function denyApproval(s: Settings, id: string, reason: string): Promise<ResumeOutcome> {
  return request<ResumeOutcome>(s, `/v1/approvals/${id}/deny`, {
    method: "POST",
    body: JSON.stringify({ reason }),
  });
}

// eventsURL builds the SSE endpoint URL -- consumed by lib/sse.ts via
// fetch(), never native EventSource (which can't set the auth headers this
// backend requires).
export function eventsURL(s: Settings, runId: string): string {
  return `${s.baseUrl}/v1/runs/${runId}/events`;
}
