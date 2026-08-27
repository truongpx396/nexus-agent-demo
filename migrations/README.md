Numbered SQL migrations, applied in filename order by `internal/store.Migrate`
(embedded into the binary via `embed.go`). Expand-only: a rollback is a new,
later-numbered migration, never a mutation of one already applied.

- `0001_tenants.sql` — the tenant registry, RLS'd against its own primary key.
- `0002_sessions.sql` — the session/run record; several columns (agent_id,
  harness_digest, the delegation-chain columns, plan_id) are seams for later
  phases, populated meaningfully only once those phases land.
- `0003_events.sql` — the append-only event log: RLS plus a trigger that
  rejects UPDATE/DELETE unconditionally, for every role including the owner.
- `0004_encryption_keys.sql` — per-tenant wrapped DEKs (`internal/crypto`).

Every table here enables **and forces** row-level security — `FORCE` is
required in addition to `ENABLE`, or the role that owns the tables (also the
role `nexusd` connects as) would bypass the policy entirely.
