---
paths:
  - "migrations/**"
---

# SQL migrations

- Files are `NNNN_snake_case.sql`, embedded via `//go:embed *.sql` and applied by `store.Migrate` in filename order, each in its own transaction, tracked by filename in `schema_migrations`. Use the next free number.
- Never edit a migration that has been applied or merged. A fix or rollback is a new, later-numbered migration.
- Expand/contract only: a rolling deploy runs old and new code against one database, so add first and clean up in a later release. No change may require old and new schema at the same time.
- **Every new table** carries `tenant_id uuid NOT NULL REFERENCES tenants (tenant_id)`, then `ENABLE` **and** `FORCE ROW LEVEL SECURITY`, plus a policy with both `USING` and `WITH CHECK` on `tenant_id = current_setting('app.tenant_id', true)::uuid` (copy `0002_sessions.sql`). `FORCE` is required because the owning role would otherwise bypass the policy. `nexusd migrate` prints `RLS enabled on N/M tenant tables` from `store.RLSTableCount`, which counts every `public` table except `schema_migrations` — it only reports, it does not fail, so N < M after your migration means you missed one.
- New tables get `nexus_app` DML privileges automatically from the default privileges in `0000_app_role.sql`; don't add blanket grants.
- `events` is append-only (a trigger rejects UPDATE/DELETE for every role). Derived tables are projections of the log — never a second source of truth.
- `make migrate` runs as the superuser `nexus`, direct to Postgres on :5433, and sees every tenant's rows. Verify isolation as `nexus_app` through PgBouncer (:6432); the tests that do this are the integration suite (`internal/store/isolation_integration_test.go`).
