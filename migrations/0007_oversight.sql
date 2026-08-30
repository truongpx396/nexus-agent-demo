-- The approval transaction (README.md §5, Phase 5, tasks 5.6-5.10):
-- internal/oversight/approval.go. canonical_digest binds the approval to the
-- EXACT (tool_id, input) the permission chain asked about (internal/tools/
-- pipeline.go's CanonicalDigest, steps 5/8) — internal/tools.ExecuteApproved
-- re-verifies this digest before ever executing, refusing with a typed
-- approval_mismatch rather than silently re-asking (task 5.7). context_package
-- is the decision-ready rendering an approver sees: never a bare UUID.
CREATE TABLE approvals (
    approval_id        uuid PRIMARY KEY,
    tenant_id           uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id          uuid NOT NULL REFERENCES sessions (session_id),
    tool_use_event_id   uuid NOT NULL,             -- the tool_use this approval gates; pair_ref target on resume
    tool_id             text NOT NULL,
    ask_kind            text NOT NULL,              -- "once" | "session" | "multi_party" (internal/permissions.AskKind)
    canonical_digest    bytea NOT NULL,             -- bound at Create; REBOUND by GrantModified to the new input
    context_package     jsonb NOT NULL,             -- rendered tool_id/effect_class/input fields for the approver
    assignee             text NOT NULL,              -- defaults to the session's owning user_id; no RBAC/directory yet (Phase 7)
    status               text NOT NULL DEFAULT 'pending', -- pending | granted | granted_modified | denied | expired | invalidated
    granted_input        jsonb,                      -- set only by GrantModified: the approver's substituted input
    expires_at            timestamptz NOT NULL,
    decided_at            timestamptz,
    decided_by            text,
    reason                text,                       -- set on deny/expire/invalidate
    created_at            timestamptz NOT NULL DEFAULT now(),

    CHECK (status IN ('pending', 'granted', 'granted_modified', 'denied', 'expired', 'invalidated'))
);

CREATE INDEX approvals_session_idx ON approvals (session_id);
CREATE INDEX approvals_pending_idx ON approvals (tenant_id, status) WHERE status = 'pending';

ALTER TABLE approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals FORCE ROW LEVEL SECURITY;

CREATE POLICY approvals_isolation ON approvals
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- Input requests (task 5.9): agent-to-human PULL, distinct from an approval
-- in every way that matters — an answer carries ZERO authorization value
-- (internal/oversight/input.go never satisfies a permission-chain Ask), and
-- on_expiry can resolve to a recorded default assumption instead of a
-- refusal, which an approval's expiry (always a denial) never does.
CREATE TABLE input_requests (
    input_request_id    uuid PRIMARY KEY,
    tenant_id            uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id           uuid NOT NULL REFERENCES sessions (session_id),
    question             text NOT NULL,
    schema               jsonb,                      -- optional structured-answer schema
    on_expiry            text NOT NULL DEFAULT 'expire', -- "expire" | "default"
    default_assumption   jsonb,                      -- used when on_expiry = 'default'
    status                text NOT NULL DEFAULT 'pending', -- pending | answered | expired | invalidated
    answer                jsonb,
    used_default          boolean NOT NULL DEFAULT false,
    expires_at            timestamptz NOT NULL,
    decided_at            timestamptz,
    reason                text,
    created_at            timestamptz NOT NULL DEFAULT now(),

    CHECK (status IN ('pending', 'answered', 'expired', 'invalidated')),
    CHECK (on_expiry IN ('expire', 'default'))
);

CREATE INDEX input_requests_session_idx ON input_requests (session_id);
CREATE INDEX input_requests_pending_idx ON input_requests (tenant_id, status) WHERE status = 'pending';

ALTER TABLE input_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE input_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY input_requests_isolation ON input_requests
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
