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
    hash    TEXT,
    -- seq anchors each mirrored event to its log position (not insertion id), so the
    -- mirror can reproduce ordering/gaps. migrateEventSeq() ALTERs this in on a DB that
    -- predates the column; a fresh DB declares it here. -1 = "seq unknown" (legacy rows).
    seq     INTEGER NOT NULL DEFAULT -1
);
CREATE INDEX IF NOT EXISTS idx_events_task ON events(task);

-- (scope, project, mkey) is the logical key: a "key" must actually be a key, so a
-- changed value for the same key REPLACES rather than accumulating a stale duplicate
-- row (PutMemory upserts ON CONFLICT). The UNIQUE constraint lands on a fresh DB here;
-- a DB created before this constraint is migrated to it in Go (Store.migrateMemory),
-- which collapses any pre-existing duplicates first since the constraint is enforced.
CREATE TABLE IF NOT EXISTS memory (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    scope   TEXT NOT NULL,              -- 'project' | 'global'
    project TEXT NOT NULL DEFAULT '',
    mkey    TEXT NOT NULL,
    mvalue  TEXT NOT NULL,
    created TEXT NOT NULL,
    UNIQUE (scope, project, mkey)
);
CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory(scope, project);

CREATE TABLE IF NOT EXISTS tasks (
    id      TEXT PRIMARY KEY,
    goal    TEXT NOT NULL,
    status  TEXT NOT NULL,
    created TEXT NOT NULL,
    updated TEXT NOT NULL,
    detail  TEXT NOT NULL DEFAULT ''  -- JSON run-state snapshot for restart durability (P5-T03)
);

-- The `detail` column above lands automatically on a fresh DB. For a DB created
-- before P5-T03 the CREATE above is a no-op (IF NOT EXISTS), so an additive
-- migration adds the column in Go (Store.migrate) — guarded by pragma_table_info
-- because SQLite's ALTER TABLE ADD COLUMN has no IF NOT EXISTS form.

-- Experience-layer projection (EXP-T02). These three tables are the persistent
-- backing for the closed-loop experience layer: a *derived* read model rebuilt
-- from the append-only event log, never a source of truth. They are additive and
-- default-empty — with no projection written, every existing events/memory/tasks
-- query is byte-identical. exp_meta tracks the rebuild watermark (last event seq
-- folded in, whether the hash chain verified, when) so a projection can be torn
-- down and replayed from the log without mutating history (I5).
CREATE TABLE IF NOT EXISTS exp_backend_standing (
    class      TEXT NOT NULL,   -- task class / bucket the standing is scoped to
    backend    TEXT NOT NULL,   -- native | codex | claude-code | ...
    races      INTEGER NOT NULL DEFAULT 0,
    passes     INTEGER NOT NULL DEFAULT 0,
    cost_usd   REAL    NOT NULL DEFAULT 0,
    latency_ns INTEGER NOT NULL DEFAULT 0,
    last_seen  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (class, backend)
);

CREATE TABLE IF NOT EXISTS exp_config_standing (
    config     TEXT PRIMARY KEY,
    pass_rate  REAL    NOT NULL DEFAULT 0,
    total_cost REAL    NOT NULL DEFAULT 0,
    cases      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS exp_meta (
    id         INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton row
    source_seq INTEGER NOT NULL DEFAULT 0,          -- last event seq folded into the projection
    chain_ok   INTEGER NOT NULL DEFAULT 0,          -- 1 if the event hash chain verified at rebuild
    rebuilt_at TEXT    NOT NULL DEFAULT ''
);

-- Standing-objectives backlog (AUTO-T01, Pillar 7). The durable backing for the
-- autonomy daemon's idle self-service queue: each row is one operator-authored
-- standing intent the agent pulls from when it has no foreground work. Additive
-- and default-empty — with no objective enqueued, every existing query above is
-- byte-identical and the backlog source stays off (the default-off contract).
-- Goal is operator-authored, inert data; the store never interprets it (I7).
-- min_period_ns / retry_period_ns store time.Duration as nanoseconds; last_run and
-- last_success are RFC3339Nano UTC (or '' for the zero time, matching formatTS/parseTS);
-- enabled is 0/1. retry_period_ns and last_success land on a fresh DB here; a DB created
-- before they existed is ALTERed in Go (Store.migrateObjectiveCadence), guarded by
-- pragma_table_info the same way as events.seq — additive and idempotent every Open.
CREATE TABLE IF NOT EXISTS objectives (
    id              TEXT PRIMARY KEY,
    goal            TEXT    NOT NULL DEFAULT '',
    priority        INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,   -- 1 enabled, 0 paused (inert, retained)
    min_period_ns   INTEGER NOT NULL DEFAULT 0,   -- minimum spacing between SUCCESSFUL runs (time.Duration ns)
    retry_period_ns INTEGER NOT NULL DEFAULT 0,   -- shorter spacing after an unverified run; 0 = fall back to min_period_ns
    last_run        TEXT    NOT NULL DEFAULT '',   -- RFC3339Nano UTC, '' = never run (debounce clock)
    last_success    TEXT    NOT NULL DEFAULT ''    -- RFC3339Nano UTC, '' = never verified (success-cadence clock)
);
