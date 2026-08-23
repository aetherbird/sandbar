-- Enforce uniqueness of (thread_id, seq) so that concurrent AppendMessage calls
-- can never persist duplicate seq values. This is the schema-level guarantee that
-- protects compression FirstKeptSeq integrity under concurrent server sessions.
-- The existing non-unique index idx_messages_thread_seq is left in place (it
-- serves the same query patterns); this unique index adds the hard constraint.
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_thread_seq_unique
ON messages(thread_id, seq);
