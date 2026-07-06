// Package store is the SQLite-backed persistent backbone (P4-T01): events,
// cross-project memory, and tasks. It wraps database/sql with hand-written typed
// queries (simpler than a sqlc codegen step, and no extra build toolchain) and
// uses modernc.org/sqlite — a pure-Go driver, so the cross-compiled release
// matrix keeps CGO_ENABLED=0. SQLite is the first sanctioned dependency (I6),
// scoped to this package.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed db/schema.sql
var schema string

// Store is a handle to the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and runs migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping store: %w", err)
	}
	// Durability + concurrency hardening. WAL keeps the secondary event mirror
	// crash-safe and lets a reader run alongside the single writer; synchronous=
	// NORMAL fsyncs at every checkpoint (the safe pairing with WAL — only the last
	// committed txn is at risk on OS/power loss, never corruption); busy_timeout
	// makes a contended write wait rather than fail with SQLITE_BUSY. A single
	// writer connection avoids the modernc driver's pool racing two writers.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate runs the additive, idempotent migrations the embedded schema cannot
// express. SQLite's CREATE TABLE IF NOT EXISTS never alters an existing table, so
// a column added after a DB was first created must be ALTERed in. ALTER TABLE ADD
// COLUMN has no IF NOT EXISTS form, so each add is guarded by pragma_table_info —
// making Open safe to run every start (no error on an already-migrated DB) and
// safe on a DB that predates the column (P5-T03 restart durability).
func (s *Store) migrate(ctx context.Context) error {
	has, err := s.hasColumn(ctx, "tasks", "detail")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE tasks ADD COLUMN detail TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add tasks.detail: %w", err)
		}
	}
	if err := s.migrateMemoryUnique(ctx); err != nil {
		return err
	}
	if err := s.migrateEventSeq(ctx); err != nil {
		return err
	}
	if err := s.migrateObjectiveCadence(ctx); err != nil {
		return err
	}
	return nil
}

// migrateEventSeq adds the events.seq column that anchors each mirrored event to its
// authoritative position in the append-only log. Without it the SQLite mirror keeps
// insertion order but LOSES the Seq anchor, so the mirror cannot reproduce ordering or
// detect a GAP (a dropped event): id is a local auto-increment, not the log's Seq. The
// add is additive and idempotent — guarded by hasColumn because SQLite's ALTER TABLE
// ADD COLUMN has no IF NOT EXISTS form — so Open is safe every start and safe on a DB
// that predates the column. Legacy rows get the DEFAULT (-1 = "seq unknown"), which a
// gap check reads as "not anchored" rather than mistaking it for real position 0.
func (s *Store) migrateEventSeq(ctx context.Context) error {
	has, err := s.hasColumn(ctx, "events", "seq")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE events ADD COLUMN seq INTEGER NOT NULL DEFAULT -1`); err != nil {
			return fmt.Errorf("add events.seq: %w", err)
		}
	}
	return nil
}

// migrateMemoryUnique brings a pre-UNIQUE memory table up to the (scope, project,
// mkey) uniqueness contract. CREATE TABLE IF NOT EXISTS never alters an existing
// table and SQLite cannot ADD a UNIQUE constraint in place, so a DB created before
// the constraint is rebuilt: collapse duplicate keys (keep the newest row per logical
// key) into a fresh table that carries the constraint, then swap it in. The detection
// is the presence of the auto-named UNIQUE index SQLite creates for the constraint —
// absent ⇒ this is a legacy table. A fresh DB already has it (schema.sql), so this is
// a no-op there, keeping Open idempotent.
func (s *Store) migrateMemoryUnique(ctx context.Context) error {
	has, err := s.hasMemoryUnique(ctx)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	// Rebuild in a transaction so a crash mid-migration leaves the original intact.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate memory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmts := []string{
		`CREATE TABLE memory_new (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			scope   TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '',
			mkey    TEXT NOT NULL,
			mvalue  TEXT NOT NULL,
			created TEXT NOT NULL,
			UNIQUE (scope, project, mkey)
		)`,
		// Keep the newest row per (scope, project, mkey): the max(id) wins, carrying
		// the latest value/created — the same "changed value replaces" semantics the
		// new upsert enforces going forward.
		`INSERT INTO memory_new (id, scope, project, mkey, mvalue, created)
		 SELECT id, scope, project, mkey, mvalue, created FROM memory
		 WHERE id IN (SELECT max(id) FROM memory GROUP BY scope, project, mkey)`,
		`DROP TABLE memory`,
		`ALTER TABLE memory_new RENAME TO memory`,
		`CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory(scope, project)`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate memory: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate memory: commit: %w", err)
	}
	return nil
}

// hasMemoryUnique reports whether the memory table already carries a UNIQUE index
// over (scope, project, mkey) — the marker that the table is at the current schema.
func (s *Store) hasMemoryUnique(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA index_list(memory)`)
	if err != nil {
		return false, fmt.Errorf("index_list(memory): %w", err)
	}
	// Collect the candidate UNIQUE-constraint index names FIRST, then close this
	// cursor BEFORE issuing the per-index PRAGMA in indexCoversMemoryKey. The store
	// runs on a single SQLite connection, so querying while this cursor is still open
	// would wait forever for the connection the cursor holds (a pool self-deadlock).
	// PRAGMA index_list columns: seq, name, unique, origin, partial.
	var candidates []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan index_list: %w", err)
		}
		// origin "u" marks an index created for a UNIQUE constraint (vs "c" CREATE
		// INDEX or "pk" primary key).
		if unique == 1 && origin == "u" {
			candidates = append(candidates, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	for _, name := range candidates {
		if s.indexCoversMemoryKey(ctx, name) {
			return true, nil
		}
	}
	return false, nil
}

// indexCoversMemoryKey reports whether the named index is exactly the (scope,
// project, mkey) constraint, so a future unrelated unique index can't be mistaken
// for it.
func (s *Store) indexCoversMemoryKey(ctx context.Context, index string) bool {
	rows, err := s.db.QueryContext(ctx, `PRAGMA index_info(`+quoteIdent(index)+`)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var seqno, cid int
		var name string
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return false
		}
		cols = append(cols, name)
	}
	if len(cols) != 3 {
		return false
	}
	return cols[0] == "scope" && cols[1] == "project" && cols[2] == "mkey"
}

// quoteIdent double-quotes a SQLite identifier so an auto-generated index name
// (e.g. "sqlite_autoindex_memory_1") is safe to interpolate into a PRAGMA, which
// cannot take a bound parameter.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// hasColumn reports whether table already has a column of the given name. It uses
// pragma_table_info so the migration is a data lookup, not a parse of CREATE SQL.
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const tsFmt = time.RFC3339Nano

// Event mirrors an eventlog record for persistence. Seq is the event's authoritative
// position in the append-only log (eventlog.Event.Seq) — persisting it lets the SQLite
// mirror reproduce ordering and detect a GAP (a dropped event) rather than relying on
// the local auto-increment id, which is a per-row counter, not the log's position. A
// legacy row written before the seq column exists reads back as -1 ("seq unknown").
type Event struct {
	Seq     uint64
	Time    time.Time
	Task    string
	Kind    string
	Backend string
	Detail  string // JSON
	Prev    string
	Hash    string
}

// InsertEvent appends an event, persisting its Seq anchor alongside the chain fields
// so the mirror can reproduce log ordering / gaps (not just insertion order).
func (s *Store) InsertEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, task, kind, backend, detail, prev, hash, seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Time.UTC().Format(tsFmt), e.Task, e.Kind, e.Backend, e.Detail, e.Prev, e.Hash, int64(e.Seq))
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// EventsByTask returns a task's events ordered by their log Seq (the authoritative
// order), falling back to insertion id so legacy rows whose seq is the -1 sentinel
// still return deterministically.
func (s *Store) EventsByTask(ctx context.Context, task string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, task, kind, backend, detail, prev, hash, seq FROM events WHERE task = ? ORDER BY seq, id`, task)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts string
		var seq int64
		if err := rows.Scan(&ts, &e.Task, &e.Kind, &e.Backend, &e.Detail, &e.Prev, &e.Hash, &seq); err != nil {
			return nil, err
		}
		if seq >= 0 {
			e.Seq = uint64(seq)
		}
		e.Time, _ = time.Parse(tsFmt, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Memory is one stored fact, scoped to a project or global.
type Memory struct {
	ID      int64
	Scope   string // "project" | "global"
	Project string
	Key     string
	Value   string
	Created time.Time
}

// PutMemory inserts or updates a memory record keyed by (scope, project, mkey),
// returning the affected row's id. The "key" is a real key: a changed value for an
// existing (scope, project, mkey) REPLACES the row (ON CONFLICT) rather than leaving a
// stale duplicate behind, so recall never surfaces both the old and current value for
// the same key.
//
// The id comes from RETURNING id, evaluated for BOTH the insert and the ON CONFLICT
// UPDATE branch, so the caller always gets the id of THIS logical row. LastInsertId is
// unreliable here: on the update branch no new row is inserted, so it returns the
// connection's LAST successful insert rowid — which, on the store's single shared
// connection, may be an UNRELATED prior row. RETURNING closes that stale-rowid hole.
func (s *Store) PutMemory(ctx context.Context, m Memory) (int64, error) {
	if m.Created.IsZero() {
		m.Created = time.Now().UTC()
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO memory (scope, project, mkey, mvalue, created) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(scope, project, mkey) DO UPDATE SET mvalue=excluded.mvalue, created=excluded.created
		 RETURNING id`,
		m.Scope, m.Project, m.Key, m.Value, m.Created.UTC().Format(tsFmt)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("put memory: %w", err)
	}
	return id, nil
}

// QueryMemory returns memory for a scope (and project, for project scope).
func (s *Store) QueryMemory(ctx context.Context, scope, project string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, project, mkey, mvalue, created FROM memory WHERE scope = ? AND project = ? ORDER BY id`,
		scope, project)
	if err != nil {
		return nil, fmt.Errorf("query memory: %w", err)
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var created string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Project, &m.Key, &m.Value, &created); err != nil {
			return nil, err
		}
		m.Created, _ = time.Parse(tsFmt, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Task is a durable orchestrator task record. Detail is an opaque JSON blob the
// caller owns: the multi-agent loop snapshots its integration-tip SHA + per-node
// state there so a restart can replay merged branches and re-release only the
// un-merged ready nodes (P5-T03). It is "" for a plain single task and is never
// interpreted by the store.
type Task struct {
	ID      string
	Goal    string
	Status  string
	Detail  string // opaque JSON run-state snapshot (P5-T03); "" for single tasks
	Created time.Time
	Updated time.Time
}

// UpsertTask inserts or updates a task by id. Detail is carried through verbatim
// (the store never parses it), so a writer that does not set Detail on a status
// transition would clear it — callers update the whole record (see agent.Checkpoint).
func (s *Store) UpsertTask(ctx context.Context, t Task) error {
	now := time.Now().UTC()
	if t.Created.IsZero() {
		t.Created = now
	}
	t.Updated = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, goal, status, detail, created, updated) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET goal=excluded.goal, status=excluded.status, detail=excluded.detail, updated=excluded.updated`,
		t.ID, t.Goal, t.Status, t.Detail, t.Created.UTC().Format(tsFmt), t.Updated.UTC().Format(tsFmt))
	if err != nil {
		return fmt.Errorf("upsert task: %w", err)
	}
	return nil
}

// GetTask returns a task by id; sql.ErrNoRows if absent.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	var created, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, goal, status, detail, created, updated FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.Goal, &t.Status, &t.Detail, &created, &updated)
	if err != nil {
		return Task{}, err
	}
	t.Created, _ = time.Parse(tsFmt, created)
	t.Updated, _ = time.Parse(tsFmt, updated)
	return t, nil
}

// TasksByStatus returns tasks in a given status (e.g. "running", "interrupted")
// — used to resume in-flight work after a restart (P6-T03 / P5-T03).
func (s *Store) TasksByStatus(ctx context.Context, status string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, goal, status, detail, created, updated FROM tasks WHERE status = ? ORDER BY id`, status)
	if err != nil {
		return nil, fmt.Errorf("tasks by status: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var created, updated string
		if err := rows.Scan(&t.ID, &t.Goal, &t.Status, &t.Detail, &created, &updated); err != nil {
			return nil, err
		}
		t.Created, _ = time.Parse(tsFmt, created)
		t.Updated, _ = time.Parse(tsFmt, updated)
		out = append(out, t)
	}
	return out, rows.Err()
}
