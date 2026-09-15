# nexus web

A small React + TypeScript (Vite) web app for the nexus-agent-demo REST API
(`internal/surfaces/rest`). This is Phase 11 task 11.7 — the last of several
thin surfaces (Telegram, Zalo, email, cron, MCP, and this one) added on top
of the same unmodified REST API and kernel, to demonstrate that the API was
surface-agnostic all along. **No Go code was changed to build this.**

## Running it

1. Start the backend from the repo root:

   ```sh
   make up        # postgres + pgbouncer + redis
   make migrate   # apply migrations
   make run       # signerd + nexusd (default: http://localhost:8085)
   ```

2. In this directory:

   ```sh
   npm install
   npm run dev
   ```

   This starts the Vite dev server (default `http://localhost:5173`).

3. Open the app, click **Set up identity** in the header, and fill in:
   - **Base URL** — where `nexusd` is listening (default `http://localhost:8085`).
   - **Tenant ID** / **User ID** — any UUIDs. The backend's own dev-auth
     posture (`internal/surfaces/rest/server.go`'s doc comment) reads these
     straight from the `X-Nexus-Tenant-ID` / `X-Nexus-User-ID` headers on
     every request, with no real verification — there's no login flow to
     complete. Use `make seed` output, or any UUIDs of your choosing.

   These are persisted to `localStorage` and attached as headers on every
   request from then on.

## npm scripts

- `npm run dev` — Vite dev server with HMR.
- `npm run build` — type-check (`tsc -b`) then production build to `dist/`.
- `npm run preview` — serve the production build locally.
- `npm run lint` — oxlint.

## What's here

- **Chat** (`/`, `/runs/:id`) — a chat UI over `POST /v1/runs` (new chat) and
  `GET /v1/runs/{id}` + `GET /v1/runs/{id}/events` (an existing thread):
  message bubbles, an inferred "Thinking…"/"Using {tool}" status, tool-call
  cards, a recursively-nested sub-agent view for `platform/delegate`, inline
  approval/clarification cards, and a citations panel for `platform/retrieve`
  results. The sidebar's session list comes from `GET /v1/sessions`
  (`src/lib/timeline.ts` is the event-log → chat-timeline translation).
- **Approvals** (`/approvals`, `/approvals/:id`) — a secondary, tenant-wide
  view over `GET /v1/approvals`, independent of any one thread; renders the
  *decision-ready* `context` field (`tool_id` / `effect_class` / `input`),
  never a bare approval UUID. Grant (with optional modified-input JSON) and
  Deny actions.

### SSE without `EventSource`

Auth is a bearer token (`Authorization: Bearer <token>`, minted with
`nexusd token --tenant=<name>`), read fresh per request. The browser's
native `EventSource` API cannot set custom headers, so it can't be used
against this endpoint. Instead, `src/lib/sse.ts` + `src/lib/useRunEvents.ts`
implement SSE-over-`fetch`: they open the stream with `fetch(url, {
headers })`, read `response.body.getReader()`, and manually parse
`event: <type>\ndata: <json>\n\n` frames out of the decoded text.

## CORS

`internal/surfaces/rest`'s `Handler()` wraps every `/v1/*` route in a small
`withCORS` middleware (`server.go`) that reflects the request's `Origin` and
answers the browser's own preflight `OPTIONS` request — needed the moment
this app is served from a different origin/port than `nexusd` (the normal
local-dev shape: Vite on `:5173`, `nexusd` on `:8085`). Permissive by design
and safe for a bearer-token API: the token is something the client chooses
to attach, never a cookie the browser attaches automatically, so there's no
CSRF exposure a stricter allowlist would actually close.
