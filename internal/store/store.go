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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const tsFmt = time.RFC3339Nano

// Event mirrors an eventlog record for persistence.
type Event struct {
	Time    time.Time
	Task    string
	Kind    string
	Backend string
	Detail  string // JSON
	Prev    string
	Hash    string
}

// InsertEvent appends an event.
func (s *Store) InsertEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, task, kind, backend, detail, prev, hash) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Time.UTC().Format(tsFmt), e.Task, e.Kind, e.Backend, e.Detail, e.Prev, e.Hash)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// EventsByTask returns a task's events in insertion order.
func (s *Store) EventsByTask(ctx context.Context, task string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, task, kind, backend, detail, prev, hash FROM events WHERE task = ? ORDER BY id`, task)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&ts, &e.Task, &e.Kind, &e.Backend, &e.Detail, &e.Prev, &e.Hash); err != nil {
			return nil, err
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

// PutMemory inserts a memory record and returns its id.
func (s *Store) PutMemory(ctx context.Context, m Memory) (int64, error) {
	if m.Created.IsZero() {
		m.Created = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO memory (scope, project, mkey, mvalue, created) VALUES (?, ?, ?, ?, ?)`,
		m.Scope, m.Project, m.Key, m.Value, m.Created.UTC().Format(tsFmt))
	if err != nil {
		return 0, fmt.Errorf("put memory: %w", err)
	}
	return res.LastInsertId()
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

// Task is a durable orchestrator task record.
type Task struct {
	ID      string
	Goal    string
	Status  string
	Created time.Time
	Updated time.Time
}

// UpsertTask inserts or updates a task by id.
func (s *Store) UpsertTask(ctx context.Context, t Task) error {
	now := time.Now().UTC()
	if t.Created.IsZero() {
		t.Created = now
	}
	t.Updated = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, goal, status, created, updated) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET goal=excluded.goal, status=excluded.status, updated=excluded.updated`,
		t.ID, t.Goal, t.Status, t.Created.UTC().Format(tsFmt), t.Updated.UTC().Format(tsFmt))
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
		`SELECT id, goal, status, created, updated FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.Goal, &t.Status, &created, &updated)
	if err != nil {
		return Task{}, err
	}
	t.Created, _ = time.Parse(tsFmt, created)
	t.Updated, _ = time.Parse(tsFmt, updated)
	return t, nil
}
