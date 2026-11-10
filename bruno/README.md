# Bruno collection

The `nexusd` REST API as a [Bruno](https://www.usebruno.com) collection: every authenticated route, the three probes, request bodies, assertions and a note on each request's behaviour. It is plain text, so it diffs and reviews like code.

## Use it

1. Start the stack: `make up && make migrate && make seed && make run`.
2. In Bruno, **Open Collection** and choose this folder, then select the `local` environment.
3. Mint a token and paste it into the `token` secret variable (secrets stay in Bruno's local store, never in the repo):

   ```sh
   make -s token        # prints tenant_id, user_id and token
   ```

4. Run `Create run`, then follow it with `Get run`, `Stream run events (SSE)` and the rest. `baseUrl` is `http://localhost:8085`; change it in the environment if `NEXUS_HTTP_ADDR` differs.

Each `make token` mints a **new user**, and sessions belong to a user, so keep one token for a session of work, or mint with `./bin/nexusd --dev token --tenant=acme --user=<uuid>` to stay the same user.

## Variables

| Variable | Kind | Set by |
|---|---|---|
| `baseUrl` | environment | you; default `http://localhost:8085` |
| `token` | environment, secret | you, from `make token` |
| `granteeId` | environment | you; a user id for `Request content access grant`. Your own `user_id` from `make token` lets one token run the whole grant flow |
| `runId` | runtime | `Create run` |
| `chatRunId` | runtime | `Create run (conversational)`; used by `Steer run` and `Cancel run` |
| `approvalId` | runtime | `List pending approvals` (first entry) |
| `forkRunId` | runtime | `Fork run` |

A runtime variable wins over an environment variable of the same name, so you can also paste an id into the environment to work on an existing run.

## What is covered

| Folder | Routes |
|---|---|
| `runs` | `POST /v1/runs` (one-shot and conversational), `GET /v1/runs/{id}`, `GET /v1/runs/{id}/events` (SSE), `GET /v1/sessions` |
| `run-control` | `POST /v1/runs/{id}/steer`, `/autonomy`, `/fork`, `/cancel` |
| `approvals` | `GET /v1/approvals`, `GET /v1/approvals/{id}`, `POST /v1/approvals/{id}/grant` (as proposed and with a modified input), `/deny` |
| `content-access` | `POST /v1/sessions/{id}/content-access-grants`, `GET /v1/sessions/{id}/content-access-grants/read` |
| `ops` | `GET /healthz`, `/readyz`, `/metrics`, unauthenticated |

Not covered: the Telegram, Zalo and email webhooks (`POST /v1/webhooks/{telegram,zalo,email}/{tenant_id}`). They are authenticated by a per-tenant secret or an HMAC over the raw body, not by a bearer token, so they do not fit this collection's auth.

**Approvals need a model that proposes a tool call.** An approval exists only when a `supervised` run asks to use a tool with a non-read-only effect. The dev fake provider never does, so `List pending approvals` is empty until `NEXUS_PROVIDER` points at a real model, and the requests that act on an approval (`Get`, `Grant`, `Deny`) fail with `404` until then.

## Run it from the command line

Requests tagged `smoke` are the ones that hold on any dev stack, so they make a quick end-to-end check of the API. The approval requests that need a pending approval are left out of it.

```sh
out=$(make -s token)
cd bruno
npx @usebruno/cli@4.3.0 run -r --env local --tags smoke \
  --env-var token="$(awk '/^token:/ {print $2}' <<<"$out")" \
  --env-var granteeId="$(awk '/^user_id:/ {print $2}' <<<"$out")"
```

Requests run in folder order, which is what lets `Create run` feed the ones after it. When you change or add a route, add or update its request in the same change.
