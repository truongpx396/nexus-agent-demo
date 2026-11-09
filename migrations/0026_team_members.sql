-- A team's roster moves off the session aggregate and into the feature that owns
-- it (spec 005 T037). sessions.team_id was a column the teams feature stamped on
-- every member's row, with a foreign key from the shared aggregate to a feature
-- table: extracting teams would have meant changing the schema of every session.
-- The roster now lives in team_members, keyed by the member's session id, and
-- `sessions` no longer knows teams exist.
--
-- Not expand/contract (.claude/rules/migrations.md): this creates the table,
-- backfills it and drops the column in one transaction, because nothing has been
-- deployed, so there is no old release to keep running against the old shape. Once
-- something is deployed, a change like this one has to be split across releases.
--
-- session_id deliberately has no foreign key to sessions: a feature references the
-- aggregate by value, so the feature can be lifted out of this database without
-- schema surgery (migration 0029 removes the rest of those references).
CREATE TABLE team_members (
    session_id  uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenants (tenant_id),
    team_id     uuid NOT NULL REFERENCES teams (team_id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX team_members_team_idx ON team_members (team_id);

ALTER TABLE team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE team_members FORCE ROW LEVEL SECURITY;

CREATE POLICY team_members_isolation ON team_members
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- migrate runs as the owner, which bypasses row-level security, so this copies
-- every tenant's members.
INSERT INTO team_members (session_id, tenant_id, team_id, created_at)
SELECT session_id, tenant_id, team_id, created_at
FROM sessions
WHERE team_id IS NOT NULL;

-- Dropping the column drops sessions_team_id_idx and the foreign key to teams.
ALTER TABLE sessions DROP COLUMN team_id;
