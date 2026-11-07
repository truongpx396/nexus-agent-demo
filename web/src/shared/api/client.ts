// The REST client every feature's api.ts is built on. Every request attaches a
// bearer token (Settings.token, minted out of band via `nexusd token`) -- the
// backend verifies it and derives tenant/user identity from its claims
// (README task 13.1).
//
// Every non-2xx response is a plain-text body (Go's http.Error), never a
// JSON error envelope -- ApiError below carries that text verbatim.
import type { Settings } from "../settings/settings";

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
    Authorization: `Bearer ${s.token}`,
  };
}

export async function request<T>(s: Settings, path: string, init?: RequestInit): Promise<T> {
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
