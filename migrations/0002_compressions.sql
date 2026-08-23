-- Compression persistence: store compression summaries so they survive across turns.
-- Each row represents one compression event. The latest record per thread is the
-- active summary injected by buildMessages(). Older records are kept for audit.

CREATE TABLE IF NOT EXISTS compressions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    summary TEXT NOT NULL,
    first_kept_seq INTEGER NOT NULL,
    compressed_message_count INTEGER NOT NULL DEFAULT 0,
    pruned_tool_outputs INTEGER NOT NULL DEFAULT 0,
    before_tokens INTEGER NOT NULL DEFAULT 0,
    after_tokens INTEGER NOT NULL DEFAULT 0,
    budget_tokens INTEGER NOT NULL DEFAULT 0,
    summary_model_alias TEXT NOT NULL DEFAULT '',
    summary_model_id TEXT NOT NULL DEFAULT '',
    summary_prompt_tokens INTEGER NOT NULL DEFAULT 0,
    summary_completion_tokens INTEGER NOT NULL DEFAULT 0,
    summary_total_tokens INTEGER NOT NULL DEFAULT 0,
    fallback_used INTEGER NOT NULL DEFAULT 0,
    fallback_reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_compressions_thread_created
ON compressions(thread_id, created_at DESC);
