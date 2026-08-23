-- Durable per-thread plans. A thread owns at most one active plan, while the
-- ordered todo rows hold the plan's actionable steps. revision lets UIs apply
-- snapshots/events deterministically and next_todo_id prevents ID reuse after
-- an item is removed or a plan is replaced.

CREATE TABLE IF NOT EXISTS plans (
    thread_id       TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    revision        INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    next_todo_id    INTEGER NOT NULL DEFAULT 1 CHECK (next_todo_id >= 1),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS todos (
    thread_id       TEXT NOT NULL REFERENCES plans(thread_id) ON DELETE CASCADE,
    id              TEXT NOT NULL CHECK (length(trim(id)) > 0),
    content         TEXT NOT NULL CHECK (length(trim(content)) > 0),
    status          TEXT NOT NULL CHECK (status IN ('pending', 'in_progress', 'completed', 'cancelled')),
    position        INTEGER NOT NULL CHECK (position >= 1),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (thread_id, id),
    UNIQUE (thread_id, position)
);

CREATE INDEX IF NOT EXISTS idx_todos_thread_status_position
ON todos(thread_id, status, position);
