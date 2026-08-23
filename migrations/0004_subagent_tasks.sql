-- Subagent task persistence for resumable delegated work.
-- Each row represents one delegate_task invocation or a resumed run.
-- messages_json stores the serialized conversation history so a failed or
-- interrupted subagent can be resumed with full context instead of restarted.

CREATE TABLE IF NOT EXISTS subagent_tasks (
    id              TEXT PRIMARY KEY,           -- UUIDv4 task identifier
    goal            TEXT NOT NULL,              -- original goal passed to delegate_task
    context         TEXT NOT NULL DEFAULT '',    -- original context string
    model_alias     TEXT NOT NULL,              -- model used by the subagent
    messages_json   TEXT NOT NULL,              -- serialized []openai.ChatCompletionMessage
    turn            INTEGER NOT NULL DEFAULT 0, -- completed turns
    max_turns       INTEGER NOT NULL DEFAULT 30,
    status          TEXT NOT NULL DEFAULT 'running', -- running | completed | interrupted | failed
    result          TEXT NOT NULL DEFAULT '',    -- accumulated assistant text / final summary
    files_read      TEXT NOT NULL DEFAULT '[]',  -- JSON array of read file paths
    files_written   TEXT NOT NULL DEFAULT '[]',  -- JSON array of written/modified file paths
    commands_run    TEXT NOT NULL DEFAULT '[]',   -- JSON array of shell commands
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_subagent_tasks_status
ON subagent_tasks(status);
