-- Workspace awareness: record the working directory a thread belongs to so the
-- CLI /resume picker can group sessions by directory instead of mixing
-- conversations from every workspace (and the web UI's default) into one list.
--
-- Legacy threads created before this migration get '' (unknown workspace).
-- The migration runner records applied migrations in schema_migrations, so this
-- ALTER runs exactly once per database — no IF NOT EXISTS is needed here.
ALTER TABLE threads ADD COLUMN workspace TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_threads_workspace ON threads(workspace);
