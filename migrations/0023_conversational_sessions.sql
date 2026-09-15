-- Opt-in multi-turn conversation continuity (kernel.RunConfig.
-- Conversational): a plain content/empty turn on a conversational session
-- pauses (store.SessionStatusAwaitingInput, EventAwaitingInput) instead of
-- terminating, and internal/runctl.Control.ResumeConversation continues the
-- SAME session on the next message — same audit trail, same cost ceiling,
-- same Rule-of-Two taint state, instead of a client stitching separate
-- sessions together. conversational is pinned at session creation, exactly
-- like autonomy_level; false (every session before this migration, and
-- every non-web caller after it) reproduces the pre-existing one-task-
-- per-session behavior exactly.
--
-- updated_at is the idle-conversation sweep's own signal
-- (cmd/nexusd/background.go): how long a session has sat in
-- SessionStatusAwaitingInput with nobody answering, before the sweep ends
-- it via kernel.TerminalIdleTimeout. Written by store.UpdateSessionStatus
-- on every status transition, same-transaction like every other projection
-- column on this table (0002_sessions.sql's own doc comment).
ALTER TABLE sessions ADD COLUMN conversational boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

-- The sweep's own query shape: every awaiting_input session, oldest first.
CREATE INDEX sessions_awaiting_input_updated_at_idx ON sessions (updated_at)
    WHERE status = 'awaiting_input';
