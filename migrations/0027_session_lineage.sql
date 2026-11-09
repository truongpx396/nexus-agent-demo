-- Where a session sits in the spawn tree moves off the session aggregate and into
-- the feature that bounds it (spec 005 T037). sessions carried root_session_id,
-- depth and delegation_role, stamped by delegate and by teams and read by both to
-- enforce the depth and per-run bounds; none of them is a fact about a session
-- itself. session_lineage holds them, for the sessions that were spawned.
--
-- A session nobody spawned has no row: it is its own root at depth 0, which is why
-- a thread root costs nothing to create and the code that creates one does not know
-- delegation exists. delegate's per-root count reads that with COALESCE, so a
-- delegation whose parent is the root itself still counts.
--
-- What the aggregate keeps is the one generic fact, who spawned this session:
-- parent_session_id has been a column since 0002 but nothing wrote it, which left
-- the sessions list filtering on delegation_role = 'root' to tell a top-level
-- thread from a child. Every spawner sets it now and the list filters on it being
-- empty, so the aggregate no longer needs a column named for any one feature.
--
-- Not expand/contract (.claude/rules/migrations.md): see 0026. Nothing has been
-- deployed, so the table, the backfill and the drop share one transaction.
CREATE TABLE session_lineage (
    session_id       uuid PRIMARY KEY,
    tenant_id        uuid NOT NULL REFERENCES tenants (tenant_id),
    root_session_id  uuid NOT NULL,
    depth            int  NOT NULL CHECK (depth >= 1),
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX session_lineage_root_idx ON session_lineage (root_session_id);

ALTER TABLE session_lineage ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_lineage FORCE ROW LEVEL SECURITY;

CREATE POLICY session_lineage_isolation ON session_lineage
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- migrate runs as the owner, which bypasses row-level security, so these cover
-- every tenant. A spawned session is one with depth > 0 (delegate and team members
-- are the only creators of those).
INSERT INTO session_lineage (session_id, tenant_id, root_session_id, depth, created_at)
SELECT session_id, tenant_id, root_session_id, depth, created_at
FROM sessions
WHERE depth > 0;

-- parent_session_id was never written, so recover it from the records that name the
-- parent: a delegation's parent, and a team member's coordinator. The sessions list
-- used to hide exactly these (delegation_role <> 'root'), so after this it hides
-- the same rows.
UPDATE sessions s
SET parent_session_id = d.parent_session_id
FROM delegations d
WHERE d.child_session_id = s.session_id
  AND s.parent_session_id IS NULL;

UPDATE sessions s
SET parent_session_id = t.coordinator_session_id
FROM team_members m
JOIN teams t ON t.team_id = m.team_id
WHERE m.session_id = s.session_id
  AND s.parent_session_id IS NULL;

-- Dropping the columns drops sessions_root_session_id_idx with them.
ALTER TABLE sessions
    DROP COLUMN root_session_id,
    DROP COLUMN depth,
    DROP COLUMN delegation_role;
