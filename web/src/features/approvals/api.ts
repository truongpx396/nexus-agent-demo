// Human oversight: the REST client for internal/oversight/rest.
import { request } from "../../shared/api/client";
import type { Settings } from "../../shared/settings/settings";
import type { ApprovalView, ResumeOutcome } from "./types";

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
