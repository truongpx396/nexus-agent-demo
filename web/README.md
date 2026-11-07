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
   - **Token** — a bearer token for a seeded tenant: from the repo root,
     `make -s token | awk '/^token:/ {print $2}'` (`make token` prints the
     tenant id, user id and token on three lines; tokens last 24 hours).
     There is no login flow: the backend verifies the token's signature and
     takes the principal only from its claims.

   These are persisted to `localStorage` and attached as an
   `Authorization: Bearer` header on every request from then on.

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
  (`src/features/chat/timeline.ts` is the event-log → chat-timeline translation).
- **Approvals** (`/approvals`, `/approvals/:id`) — a secondary, tenant-wide
  view over `GET /v1/approvals`, independent of any one thread; renders the
  *decision-ready* `context` field (`tool_id` / `effect_class` / `input`),
  never a bare approval UUID. Grant (with optional modified-input JSON) and
  Deny actions.

## Layout

The source is packaged by feature, mirroring the backend's own packages, not by
kind of file:

- `src/features/chat/` — the chat feature: the page, thread, message and tool
  cards, the sidebar, the run timeline and the live-event hook, plus its REST
  client (`api.ts`, for `internal/runs/rest` and `internal/runctl/rest`) and its
  wire types (`types.ts`).
- `src/features/approvals/` — human oversight: the approvals pages, the inline
  approval and clarification cards the chat thread renders, and their REST client
  and types (for `internal/oversight/rest`). The chat feature imports from here;
  nothing here imports from chat.
- `src/shared/` — what no one feature owns: the REST client every feature's
  `api.ts` is built on (`api/client.ts`), the settings and theme providers, and
  the generic UI (`ui/`).
- `src/App.tsx`, `src/main.tsx` and `src/index.css` — the shell and the one global
  stylesheet. The stylesheet is not split per feature: several of its classes are
  composed at runtime (the `status-*` pills), and splitting it safely needs visual
  regression tests this repo does not have.

### SSE without `EventSource`

Auth is a bearer token (`Authorization: Bearer <token>`, minted with
`nexusd token --tenant=<name>`), read fresh per request. The browser's
native `EventSource` API cannot set custom headers, so it can't be used
against this endpoint. Instead, `src/features/chat/sse.ts` + `src/features/chat/useRunEvents.ts`
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
