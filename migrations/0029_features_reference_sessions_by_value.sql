-- No feature table has a foreign key into sessions any more (spec 005 T037). Every
-- feature table carried a session_id (or a parent, child, coordinator, claimant)
-- column declared REFERENCES sessions (session_id): about twenty of them, from
-- approvals and claims to budgets and the audit chain. Each one tied that feature's
-- schema to the shared aggregate's, so lifting a feature out into its own service
-- would have meant first dismantling a web of constraints in a database it no
-- longer shared. They now reference a session by value: the id is stored, and
-- nothing in the database checks that it exists.
--
-- What stays is what is inside the aggregate itself: events.session_id (the log and
-- the session are one aggregate) and sessions.parent_session_id (a session's own
-- parent). Nothing else may name sessions in a constraint; the integration test
-- TestSessionsAggregate_NoFeatureReferencesIt fails if one does.
--
-- The trade is deliberate: the database no longer rejects a row that names a
-- session that does not exist. Every writer is acting on a session it already holds
-- (a run, a resume, a decision on its approval), and sessions are never deleted
-- (erasure is crypto-shredding, docs/constitution.md), so no cascade is lost
-- either.
--
-- Not expand/contract (.claude/rules/migrations.md): see 0026.
--
-- The loop reads the constraints from the catalogue instead of naming them, because
-- the earlier migrations left them under generated names.
DO $$
DECLARE
    c record;
BEGIN
    FOR c IN
        SELECT conrelid::regclass AS tbl, conname
        FROM pg_constraint
        WHERE contype = 'f'
          AND confrelid = 'sessions'::regclass
          AND conrelid NOT IN ('sessions'::regclass, 'events'::regclass)
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', c.tbl, c.conname);
    END LOOP;
END
$$;
