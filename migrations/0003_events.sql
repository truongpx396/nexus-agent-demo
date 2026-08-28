-- The single source of truth (FR-002, FR-003, FR-040). Append-only: no
-- UPDATE or DELETE, enforced by the trigger below rather than by convention
-- — a BEFORE trigger fires for every role, including the table owner, so
-- this holds even without a separate low-privilege runtime role.
CREATE TABLE events (
    event_id        uuid PRIMARY KEY,
    session_id      uuid NOT NULL REFERENCES sessions (session_id),
    tenant_id       uuid NOT NULL REFERENCES tenants (tenant_id),
    seq             bigint NOT NULL,
    schema_version  int NOT NULL,
    type            text NOT NULL,
    payload         bytea,              -- ciphertext under key_id (internal/crypto)
    payload_digest  bytea NOT NULL,     -- digest over PLAINTEXT; survives crypto-shredding
    key_id          text NOT NULL,      -- destroying this key IS erasure (Phase 5)
    actor           text NOT NULL,
    tool_id         text,
    pair_ref        uuid,               -- tool_result -> tool_use (THE paired-result invariant)
    model_id        text,
    trace_id        bytea,
    span_id         bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (session_id, seq)
);

CREATE INDEX events_session_seq_idx ON events (session_id, seq);
CREATE INDEX events_pair_ref_idx ON events (pair_ref) WHERE pair_ref IS NOT NULL;

-- See 0001_tenants.sql: FORCE matters for the owner IF the owner is an
-- ordinary role; nexus (the migration/owner role) is a superuser and
-- bypasses RLS regardless, so the real enforcement point is nexus_app
-- (0000_app_role.sql), the role nexusd's runtime path actually connects as.
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;

CREATE POLICY events_isolation ON events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE FUNCTION events_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'events is append-only: % is not permitted (event_id=%)', TG_OP, OLD.event_id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER events_append_only
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_forbid_mutation();

-- Belt and suspenders: nexus_app has UPDATE/DELETE on every table via
-- 0000_app_role.sql's blanket default privilege, but events must never be
-- mutable even in principle. The trigger above already blocks it
-- unconditionally; this REVOKE means the attempt is refused at the
-- privilege check, one step earlier than the trigger.
REVOKE UPDATE, DELETE ON events FROM nexus_app;
