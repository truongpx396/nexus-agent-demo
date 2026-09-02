-- Peer agent teams: shared task boards (README.md §9, Phase 9). New scope,
-- not derived from the original's 67 patterns (README §3's own note on this)
-- — it reuses this plan's existing primitives (queue claim semantics, the
-- session model, envelope reservation, taint-fold) rather than inventing a
-- second set of rules, and is bound by the same fail-closed, no-widening
-- discipline delegation (migrations/0016) already established.

-- teams: one row per fixed-roster team (task 9.1). roster is fixed at
-- creation — no mid-run recruitment, the same no-widening discipline the
-- autonomy ratchet (3.7) and skill intersection (7.4) already follow.
-- coordinator_session_id is the session whose own event log carries
-- team_created/team_ended (mirroring how a delegation's parent session
-- carries delegation_requested/delegation_returned) — it is NOT itself a
-- team member (a team member is a leaf, task 9.10; the coordinator is
-- whatever ordinary, non-team-member session decided to spin the team up).
CREATE TABLE teams (
    team_id                uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenants (tenant_id),
    name                     text NOT NULL,
    coordinator_session_id    uuid NOT NULL REFERENCES sessions (session_id),
    roster                     jsonb NOT NULL,          -- [{agent_id, task}], fixed at creation
    envelope_id                 uuid,                    -- bound once the team_envelopes row commits (below)
    status                       text NOT NULL DEFAULT 'active',
    reason                        text,
    created_at                     timestamptz NOT NULL DEFAULT now(),
    completed_at                    timestamptz,

    CHECK (status IN ('active', 'completed', 'aborted', 'ceiling_exhausted'))
);

CREATE INDEX teams_tenant_status_idx ON teams (tenant_id, status);

ALTER TABLE teams ENABLE ROW LEVEL SECURITY;
ALTER TABLE teams FORCE ROW LEVEL SECURITY;

CREATE POLICY teams_isolation ON teams
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- sessions.team_id (task 9.2): each member is an ORDINARY session — reuses
-- the session-key serial lock (6.2) for per-member concurrency, no new
-- locking primitive for the loop. Nullable: every non-team session (the
-- overwhelming majority, before and after this migration) leaves it NULL.
ALTER TABLE sessions ADD COLUMN team_id uuid REFERENCES teams (team_id);
CREATE INDEX sessions_team_id_idx ON sessions (team_id) WHERE team_id IS NOT NULL;

-- board_cards (task 9.3): RLS-scoped like every table in this system.
-- taint_state is copied from the WRITER's own Rule-of-Two engaged legs at
-- creation (8.11's copy-at-spawn pattern, applied to a card instead of a
-- child session) — a JSON array of 3 booleans, the same [3]bool shape
-- internal/tools/pipeline.go's own TaintState.Engaged already uses. A reader
-- folds it into ITS OWN taint state at READ time (task 9.6), never at write
-- time — the one genuinely new mechanism this phase needs.
-- injection_scan_status reuses the same fail-closed three-state discipline
-- internal/memory's own Screen already applies to a memory file (task 9.7):
-- a flagged card's body is never surfaced to another peer's context.
CREATE TABLE board_cards (
    card_id                    uuid PRIMARY KEY,
    tenant_id                   uuid NOT NULL REFERENCES tenants (tenant_id),
    team_id                      uuid NOT NULL REFERENCES teams (team_id),
    title                         text NOT NULL,
    body                            text NOT NULL DEFAULT '',
    status                           text NOT NULL DEFAULT 'open',
    taint_state                       jsonb NOT NULL DEFAULT '[false,false,false]'::jsonb,
    injection_scan_status               text NOT NULL DEFAULT 'pending',
    scan_findings                         jsonb NOT NULL DEFAULT '[]'::jsonb,
    written_by_session_id                  uuid NOT NULL REFERENCES sessions (session_id),
    claimed_by_session_id                    uuid REFERENCES sessions (session_id),
    created_at                                 timestamptz NOT NULL DEFAULT now(),
    updated_at                                   timestamptz NOT NULL DEFAULT now(),

    CHECK (status IN ('open', 'claimed', 'in_progress', 'done', 'blocked')),
    CHECK (injection_scan_status IN ('pending', 'clean', 'flagged'))
);

CREATE INDEX board_cards_team_status_idx ON board_cards (team_id, status);

ALTER TABLE board_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE board_cards FORCE ROW LEVEL SECURITY;

CREATE POLICY board_cards_isolation ON board_cards
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- team_envelopes (task 9.8): the SAME shared-ceiling pattern
-- fanout_envelopes (migrations/0016) already proves for a delegate_fanout
-- plan step (task 8.13) — reserved ONCE, before the first member starts,
-- sized to the roster's worst case, and drawn down by every member's own
-- turn-level reservation. A parallel table, not a reused one:
-- fanout_envelopes.plan_session_id is NOT NULL and means something
-- structurally different (one plan step's own driving session) from a
-- team's coordinator session, and internal/teams has no structural reason to
-- import internal/delegate just to share a row shape — this codebase's own
-- established idiom (internal/tools/builtin/delegate.go's own doc comment on
-- deliberately independent request shapes) applied here to a table instead
-- of a Go struct. Every member's own turn-level draw decrements THIS row,
-- never the tenant's Redis-backed ceiling — "no member draws an independent
-- per-tenant reservation" (task 9.8) is true by construction, not convention.
CREATE TABLE team_envelopes (
    envelope_id         uuid PRIMARY KEY,
    tenant_id             uuid NOT NULL REFERENCES tenants (tenant_id),
    team_id                 uuid NOT NULL REFERENCES teams (team_id),
    currency                  text NOT NULL DEFAULT 'USD',
    ceiling_micros              bigint NOT NULL,
    remaining_micros              bigint NOT NULL,
    member_count                    int NOT NULL,
    created_at                        timestamptz NOT NULL DEFAULT now(),

    CHECK (remaining_micros >= 0),
    CHECK (remaining_micros <= ceiling_micros)
);

CREATE INDEX team_envelopes_team_id_idx ON team_envelopes (team_id);

ALTER TABLE team_envelopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE team_envelopes FORCE ROW LEVEL SECURITY;

CREATE POLICY team_envelopes_isolation ON team_envelopes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- teams.envelope_id -> team_envelopes.envelope_id: bound now that both
-- tables exist (team_envelopes is defined after teams, above).
ALTER TABLE teams ADD CONSTRAINT teams_envelope_id_fkey
    FOREIGN KEY (envelope_id) REFERENCES team_envelopes (envelope_id);
