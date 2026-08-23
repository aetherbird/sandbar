-- Plan-mode lifecycle per thread: '' (off) → 'planning' (a plan turn is
-- requested/running) → 'pending_approval' (the plan turn completed and awaits
-- the user's decision) → 'approved' (inject the execution directive once on
-- the next turn, then back to ''). Rejection or a normal turn clears to ''.
-- Persisting it lets /plan state survive CLI restarts and session resume.
--
-- The migration runner records applied migrations in schema_migrations, so this
-- ALTER runs exactly once per database — no IF NOT EXISTS is needed here.
ALTER TABLE threads ADD COLUMN plan_mode TEXT NOT NULL DEFAULT '';
