CREATE TABLE IF NOT EXISTS threads (
    id          TEXT PRIMARY KEY,
    title       TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    model       TEXT
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id    TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    role         TEXT NOT NULL,
    content      TEXT,
    tool_call_id TEXT,
    created_at   INTEGER NOT NULL,
    seq          INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_thread_seq ON messages(thread_id, seq);

CREATE TABLE IF NOT EXISTS tool_calls (
    id          TEXT PRIMARY KEY,
    message_id  INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    tool_name   TEXT NOT NULL,
    arguments   TEXT NOT NULL,
    seq         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tool_calls_message ON tool_calls(message_id);

-- ============================================================
-- FTS5 full-text search over messages (cross-session search)
-- ============================================================
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content_rowid='id',
    content='messages'
);

-- Trigger: insert into FTS index when a message is added.
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

-- Trigger: update FTS index when a message content changes.
CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF content ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.id, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

-- Trigger: delete from FTS index when a message is removed.
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.id, old.content);
END;

-- Rebuild helper: ensure existing messages are indexed on first run.
INSERT OR IGNORE INTO messages_fts(rowid, content)
SELECT id, content FROM messages WHERE content IS NOT NULL;
