-- NilCore SQLite schema (P4-T01). The persistent backbone for the event log
-- (P4-T02), cross-project memory (P4-T03), and task durability (P6-T03).
-- Migrations are idempotent (IF NOT EXISTS) so Open can run them every start.

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      TEXT NOT NULL,
    task    TEXT,
    kind    TEXT NOT NULL,
    backend TEXT,
    detail  TEXT,           -- JSON
    prev    TEXT,           -- hash chain (P2-T06)
    hash    TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_task ON events(task);

CREATE TABLE IF NOT EXISTS memory (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    scope   TEXT NOT NULL,              -- 'project' | 'global'
    project TEXT NOT NULL DEFAULT '',
    mkey    TEXT NOT NULL,
    mvalue  TEXT NOT NULL,
    created TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory(scope, project);

CREATE TABLE IF NOT EXISTS tasks (
    id      TEXT PRIMARY KEY,
    goal    TEXT NOT NULL,
    status  TEXT NOT NULL,
    created TEXT NOT NULL,
    updated TEXT NOT NULL
);
