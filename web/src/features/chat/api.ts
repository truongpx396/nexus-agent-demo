// Runs and run control: the REST client for internal/runs/rest (create, read,
// list, stream) and internal/runctl/rest (cancel, steer, tighten autonomy, fork).
import { request } from "../../shared/api/client";
import type { Settings } from "../../shared/settings/settings";
import type {
  CreateRunRequest,
  CreateRunResponse,
  ForkView,
  GetRunResponse,
  SessionSummary,
} from "./types";

export function createRun(s: Settings, body: CreateRunRequest): Promise<CreateRunResponse> {
  const payload: CreateRunRequest = { input: body.input };
  if (body.data_label) payload.data_label = body.data_label;
  if (body.difficulty) payload.difficulty = body.difficulty;
  if (body.autonomy) payload.autonomy = body.autonomy;
  if (body.budget_usd) payload.budget_usd = body.budget_usd;
  if (body.conversational) payload.conversational = body.conversational;
  return request<CreateRunResponse>(s, "/v1/runs", {
    method: "POST",
    body: JSON.stringify(payload),
  });
}

export function getRun(s: Settings, id: string): Promise<GetRunResponse> {
  return request<GetRunResponse>(s, `/v1/runs/${id}`);
}

export function listSessions(s: Settings): Promise<SessionSummary[]> {
  return request<SessionSummary[]>(s, "/v1/sessions");
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

// eventsURL builds the SSE endpoint URL -- consumed by sse.ts via fetch(),
// never native EventSource (which can't set the auth headers this backend
// requires).
export function eventsURL(s: Settings, runId: string): string {
  return `${s.baseUrl}/v1/runs/${runId}/events`;
}
