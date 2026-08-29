-- Cost governance (README.md §5, Phase 4): reserve-then-reconcile against
-- hard ceilings, priced from a versioned, immutable price book. Every
-- amount here is an integer in currency MICROS (1 currency unit =
-- 1_000_000 micros) — internal/cost/money.go's Money type is the only
-- thing that ever computes with these columns, and that package bans
-- float32/float64 entirely (a package-local AST guard test, since
-- forbidigo can't ban a type — see .golangci.yml's own comment on this).

-- price_book: versioned, keyed (meter, subject, effective range). Entries
-- are immutable once written (internal/cost/pricebook.go's own doc
-- comment) — a price change is a NEW row with a later effective_from, which
-- is what keeps a historical cost reproducible even after prices move
-- (README task 4.3). tenant_id is present for schema consistency with
-- every other tenant-scoped table (README §4's blanket rule), even though
-- the per-tenant price OVERRIDE feature itself is out of scope (README §3,
-- pattern 65, "price overrides" — a commercial/billing concern): every
-- tenant is seeded with identical rows (cmd/nexusd's seed command).
CREATE TABLE price_book (
    price_book_id             uuid PRIMARY KEY,
    tenant_id                 uuid NOT NULL REFERENCES tenants (tenant_id),
    meter                     text NOT NULL,
    subject                   text NOT NULL DEFAULT '*',
    version                   int NOT NULL,
    currency                  text NOT NULL DEFAULT 'USD',
    price_micros_per_million  bigint NOT NULL,   -- currency-micros per ONE MILLION meter units
    effective_from            timestamptz NOT NULL,
    effective_until           timestamptz,        -- NULL = still in force
    created_at                timestamptz NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, meter, subject, version)
);

CREATE INDEX price_book_lookup_idx ON price_book (tenant_id, meter, subject, effective_from);

ALTER TABLE price_book ENABLE ROW LEVEL SECURITY;
ALTER TABLE price_book FORCE ROW LEVEL SECURITY;

CREATE POLICY price_book_isolation ON price_book
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- budgets: a hard ceiling, scoped either to a whole tenant (cross-session,
-- cross-worker — enforced via internal/cost/redis.go's epoch-marked Redis
-- counter) or to one session (the worker-local, per-run ceiling enforced
-- purely in-process, README task 4.5 — "a ceiling never depends on a round
-- trip"). scope_ref is the session_id for scope='session' and NULL for
-- scope='tenant'; at most one budget exists per (tenant, scope, scope_ref).
-- epoch only ever advances via an explicit administrative reset — no reset
-- operation ships this phase; the column is the seam.
CREATE TABLE budgets (
    budget_id       uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenants (tenant_id),
    scope           text NOT NULL,                 -- 'tenant' | 'session'
    scope_ref       uuid REFERENCES sessions (session_id),
    currency        text NOT NULL DEFAULT 'USD',
    ceiling_micros  bigint NOT NULL,
    epoch           bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CHECK (scope IN ('tenant', 'session')),
    CHECK ((scope = 'tenant') = (scope_ref IS NULL))
);

CREATE UNIQUE INDEX budgets_tenant_ceiling_idx ON budgets (tenant_id) WHERE scope = 'tenant';
CREATE UNIQUE INDEX budgets_session_ceiling_idx ON budgets (scope_ref) WHERE scope = 'session';

ALTER TABLE budgets ENABLE ROW LEVEL SECURITY;
ALTER TABLE budgets FORCE ROW LEVEL SECURITY;

CREATE POLICY budgets_isolation ON budgets
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- budget_decisions: one durable audit row per Reserve resolution — EVERY
-- resolution, including an unenforced 'skip' (README task 4.6: "an
-- unenforced ceiling is visibly distinct from a ceiling with room"). The
-- corresponding store.EventBudgetDecision is appended to the event log
-- separately by the kernel (internal/cost never writes to `events` itself —
-- see internal/cost/gate.go's doc comment on why these are two durable
-- writes, not one shared transaction).
CREATE TABLE budget_decisions (
    budget_decision_id  uuid PRIMARY KEY,
    tenant_id            uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id            uuid NOT NULL REFERENCES sessions (session_id),
    decision              text NOT NULL,     -- allow | refuse_ceiling | degrade | skip
    reason                text NOT NULL,
    budget_id             uuid REFERENCES budgets (budget_id),
    reserved_micros       bigint NOT NULL DEFAULT 0,
    currency              text NOT NULL DEFAULT 'USD',
    created_at             timestamptz NOT NULL DEFAULT now(),

    CHECK (decision IN ('allow', 'refuse_ceiling', 'degrade', 'skip'))
);

CREATE INDEX budget_decisions_session_idx ON budget_decisions (session_id);
CREATE INDEX budget_decisions_tenant_idx ON budget_decisions (tenant_id);

ALTER TABLE budget_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE budget_decisions FORCE ROW LEVEL SECURITY;

CREATE POLICY budget_decisions_isolation ON budget_decisions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- cost_records: the durable, reconciled truth of what a call actually cost
-- — one row per meter with nonzero quantity, written by Reconcile after
-- the provider stream completes (README task 4.9). unreported=true marks
-- the one row Reconcile writes when usage was UNREPORTED (task 4.7): the
-- full reserved worst case, charged because an unreliable provider must
-- not look free.
CREATE TABLE cost_records (
    cost_record_id  uuid PRIMARY KEY,
    tenant_id        uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id        uuid NOT NULL REFERENCES sessions (session_id),
    reservation_id     uuid,
    meter               text NOT NULL,
    quantity            bigint NOT NULL,
    unit                text NOT NULL,
    minor_units         bigint NOT NULL,   -- cost in currency micros
    currency            text NOT NULL DEFAULT 'USD',
    model_id            text,
    unreported          boolean NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cost_records_session_idx ON cost_records (session_id);
CREATE INDEX cost_records_tenant_idx ON cost_records (tenant_id);

ALTER TABLE cost_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE cost_records FORCE ROW LEVEL SECURITY;

CREATE POLICY cost_records_isolation ON cost_records
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
