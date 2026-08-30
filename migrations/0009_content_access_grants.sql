-- Content access grants (README.md §5, task 5.11): the only path to
-- plaintext outside a run's own audience (internal/obs/grant.go). Distinct
-- from the run's own owning user reading their own session's events over
-- REST (internal/surfaces/rest/server.go's handleEvents, gated on
-- sess.UserID == userID — pattern #51, already shipped) — a grant is for a
-- NON-owning principal (an operator/support role), and every grant AND
-- every read under it produces its own hash-chained audit receipt
-- (content_access_granted, content_accessed).
CREATE TABLE content_access_grants (
    grant_id      uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id    uuid NOT NULL REFERENCES sessions (session_id),
    grantee_id    uuid NOT NULL,     -- the non-owning principal this grant authorizes
    reason        text NOT NULL,
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX content_access_grants_session_idx ON content_access_grants (session_id);
CREATE INDEX content_access_grants_grantee_idx ON content_access_grants (tenant_id, grantee_id);

ALTER TABLE content_access_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_access_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY content_access_grants_isolation ON content_access_grants
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
