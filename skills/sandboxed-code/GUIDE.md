# Sandboxed code execution

`platform/shell` runs inside `internal/sandbox` whenever the operator has configured
an isolation backend: `NEXUS_SANDBOX=docker` (a fresh, network-isolated container per
call) or `NEXUS_SANDBOX=opensandbox` (an OpenSandbox-backed sandbox —
https://github.com/opensandbox-group/OpenSandbox — with the same deny-by-default
network policy). If neither is configured, the same tool call still runs, unsandboxed,
on the host process; check the run's own event log if you need to know which backend
actually served a particular call.

Never assume network access from inside a sandboxed command — both backends default
to deny-all egress. Fetch anything a command needs with `platform/web_fetch` or
`platform/web_crawl` first, then hand the result to the command as an argument or a
file written beforehand.

Every command runs under hard CPU/memory/wall-clock limits, not advisory ones — a
command that needs more than the default budget should be split into smaller steps
rather than assumed to eventually finish.

## Delegating execution to a sub-agent

To hand a self-contained "run and check this" task to a dedicated sub-agent instead
of running it inline, call `platform/delegate`:

```json
{
  "agent_id": "sandbox-runner",
  "task": "<the specific command, script, or test to run>",
  "scope_grant": ["platform/shell@v1", "platform/activate_skill@v1"]
}
```

As with any delegation, `scope_grant` is re-derived as a provable subset of your own
held tools — it is never trusted from the call — and the child is bound by the same
depth/concurrency/per-run limits as any other delegation.
