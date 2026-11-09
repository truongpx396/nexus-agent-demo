Numbered SQL migrations, applied in filename order by `internal/store.Migrate`
(embedded into the binary via `embed.go`). Expand-only: a rollback is a new,
later-numbered migration, never a mutation of one already applied.

- `0001_tenants.sql` — the tenant registry, RLS'd against its own primary key.
- `0002_sessions.sql` — the session/run record. Several columns it first shipped
  as seams for later features (the delegation-chain columns, plan_id, the fork
  columns, team_id) have since moved into the tables of the features that own
  them, and no feature table references it by foreign key any more (`0026`–`0029`).
- `0003_events.sql` — the append-only event log: RLS plus a trigger that
  rejects UPDATE/DELETE unconditionally, for every role including the owner.
- `0004_encryption_keys.sql` — per-tenant wrapped DEKs (`internal/crypto`).

`0026`–`0029` move what features had stamped on `sessions` into feature-owned
tables (`team_members`, `session_lineage`, `session_forks`, `plan_sessions`) and
drop the foreign keys from feature tables to `sessions`. They are not
expand/contract: each creates, backfills and drops in one transaction, because
nothing had been deployed. A change like that has to be split across releases once
something is.

Every table here enables **and forces** row-level security — `FORCE` is
required in addition to `ENABLE`, or the role that owns the tables (also the
role `nexusd` connects as) would bypass the policy entirely.
