-- Per-tenant cron schedules (README Phase 11, task 11.6): internal/surfaces/
-- cron's scheduler goroutine polls this table for due rows and submits a
-- run through the ordinary create-session-then-StartRun sequence every
-- other surface uses (README task 11.6's own proof requirement: "no new
-- admission path") — this table exists only to durably hold WHEN and WHAT,
-- never to carry any control-flow of its own.
-- budget_ceiling_micros mirrors migrations/0005_cost.sql's own
-- budgets.ceiling_micros — money is a bigint-micros column everywhere in
-- this schema, never text; a nullable value here means "no session budget,"
-- exactly like rest.createRunRequest's own optional budget_usd field.
CREATE TABLE cron_schedules (
    schedule_id            uuid PRIMARY KEY,
    tenant_id                uuid NOT NULL REFERENCES tenants (tenant_id),
    user_id                    uuid NOT NULL,
    name                         text NOT NULL,
    cron_expr                      text NOT NULL,
    input                             text NOT NULL,
    autonomy_level                     text NOT NULL DEFAULT 'supervised',
    budget_ceiling_micros                 bigint,
    enabled                                  boolean NOT NULL DEFAULT true,
    last_run_at                                timestamptz,
    next_run_at                                  timestamptz,
    created_at                                     timestamptz NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, name),
    CHECK (autonomy_level IN ('read_only', 'supervised', 'autonomous'))
);

-- Partial index: the scheduler's own due-row poll filters on
-- (enabled, next_run_at) inside an ordinary per-tenant Store.InTenantTx —
-- nexus_app is NOSUPERUSER NOBYPASSRLS (migrations/0000_app_role.sql), so
-- there is no cross-tenant query here at all; the scheduler instead lists
-- tenant IDs via the same separate admin-DSN connection
-- cmd/nexusd/main.go's own listTenantIDs already uses for the anchor loop,
-- then runs this query once per tenant, exactly like
-- anchorAndVerifyAllTenants does.
CREATE INDEX cron_schedules_due_idx ON cron_schedules (next_run_at) WHERE enabled;

ALTER TABLE cron_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE cron_schedules FORCE ROW LEVEL SECURITY;

CREATE POLICY cron_schedules_isolation ON cron_schedules
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
