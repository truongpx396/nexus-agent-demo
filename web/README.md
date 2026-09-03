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
   make run       # signerd + nexusd (default: http://localhost:8080)
   ```

2. In this directory:

   ```sh
   npm install
   npm run dev
   ```

   This starts the Vite dev server (default `http://localhost:5173`).

3. Open the app, click **Set up identity** in the header, and fill in:
   - **Base URL** — where `nexusd` is listening (default `http://localhost:8080`).
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

- **New run** (`/`) — form for `POST /v1/runs`; navigates to the run detail
  view for the new `run_id`. Also has a "jump to a run" box and a
  browser-local list of recently created runs, since the API has no
  "list runs" endpoint — only `GET /v1/runs/{id}`.
- **Run detail** (`/runs/:id`) — polls `GET /v1/runs/{id}` for status /
  terminal reason, and streams `GET /v1/runs/{id}/events` as a live
  timeline. Includes controls for cancel / steer / tighten-autonomy.
- **Approvals** (`/approvals`, `/approvals/:id`) — lists approvals from
  `GET /v1/approvals`, and renders the *decision-ready* `context` field
  (`tool_id` / `effect_class` / `input`) on the detail view, never a bare
  approval UUID. Grant (with optional modified-input JSON) and Deny actions.

### SSE without `EventSource`

The backend's only auth mechanism is the two headers above, read fresh per
request — there's no cookie or query-param auth. The browser's native
`EventSource` API cannot set custom headers, so it can't be used against
this endpoint. Instead, `src/lib/sse.ts` + `src/lib/useRunEvents.ts`
implement SSE-over-`fetch`: they open the stream with `fetch(url, {
headers })`, read `response.body.getReader()`, and manually parse
`event: <type>\ndata: <json>\n\n` frames out of the decoded text.

## CORS note

`internal/surfaces/rest` sets no CORS headers today (verified by reading
`server.go` / `oversight.go` / `runctl.go` — there is no
`Access-Control-*` header anywhere in that package). That's out of scope
for this task to change on the Go side. In practice this means:

- If the web app's origin (e.g. `http://localhost:5173`) differs from the
  backend's origin (e.g. `http://localhost:8080`), the browser will block
  cross-origin requests — and because this app sends custom headers
  (`X-Nexus-Tenant-ID`, `X-Nexus-User-ID`), the browser will first send a
  CORS preflight (`OPTIONS`) request, which this backend doesn't handle
  either.
- For local development, either:
  - run a browser with web security disabled for local testing only, e.g.
    `chromium --disable-web-security --user-data-dir=/tmp/chrome-dev`
    (do this only against a local dev backend, never a real deployment), or
  - use a browser extension that adds permissive CORS headers to
    responses from `localhost:8080` during development, or
  - serve this app's production build from the same origin/port as
    `nexusd` via a reverse proxy, so requests are same-origin.
- None of this requires or implies a Go-side change; it's a dev-environment
  workaround note, not a fix.
