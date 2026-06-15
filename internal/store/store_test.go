package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEventsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.InsertEvent(ctx, Event{Time: time.Now(), Task: "t1", Kind: "step", Detail: `{"i":1}`, Hash: "h", Prev: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertEvent(ctx, Event{Time: time.Now(), Task: "other", Kind: "x"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.EventsByTask(ctx, "t1")
	if err != nil || len(evs) != 3 {
		t.Fatalf("EventsByTask = %d, %v", len(evs), err)
	}
	if evs[0].Kind != "step" || evs[0].Hash != "h" {
		t.Errorf("event = %+v", evs[0])
	}
}

func TestMemoryRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "nilcore", Key: "convention", Value: "use stdlib"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutMemory(ctx, Memory{Scope: "global", Key: "g", Value: "v"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryMemory(ctx, "project", "nilcore")
	if err != nil || len(got) != 1 || got[0].Value != "use stdlib" {
		t.Fatalf("QueryMemory = %+v, %v", got, err)
	}
	if g, _ := s.QueryMemory(ctx, "global", ""); len(g) != 1 {
		t.Errorf("global memory = %d", len(g))
	}
}

func TestTaskUpsert(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.UpsertTask(ctx, Task{ID: "t1", Goal: "fix", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTask(ctx, Task{ID: "t1", Goal: "fix", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil || got.Status != "done" {
		t.Fatalf("GetTask = %+v, %v", got, err)
	}
	if _, err := s.GetTask(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing task = %v, want ErrNoRows", err)
	}
}

// TestTaskDetailRoundTrip proves the P5-T03 detail column carries an opaque JSON
// blob through upsert/get/by-status verbatim (the store never interprets it).
func TestTaskDetailRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	const detail = `{"tip_sha":"abc123","nodes":[{"id":"t1","state":"merged"}]}`
	if err := s.UpsertTask(ctx, Task{ID: "t1", Goal: "g", Status: "running", Detail: detail}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Detail != detail {
		t.Errorf("GetTask detail = %q, want %q", got.Detail, detail)
	}
	byStatus, err := s.TasksByStatus(ctx, "running")
	if err != nil || len(byStatus) != 1 || byStatus[0].Detail != detail {
		t.Fatalf("TasksByStatus detail = %+v, %v", byStatus, err)
	}
	// A status transition that does not re-supply Detail clears it (callers own the
	// whole record) — documents the contract the agent.Checkpoint upholds.
	if err := s.UpsertTask(ctx, Task{ID: "t1", Goal: "g", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Detail != "" {
		t.Errorf("after detail-less upsert, detail = %q, want empty", got.Detail)
	}
}

// TestMigrationAddsDetailColumn is the P5-T03 migration test: a DB created with the
// OLD tasks schema (no detail column) must, on Open, gain the column additively —
// without dropping the pre-existing row and without erroring on a re-Open
// (idempotent ALTER guarded by pragma_table_info).
func TestMigrationAddsDetailColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Stand up a legacy DB by hand: the pre-P5-T03 tasks table had no detail column.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE tasks (
		id TEXT PRIMARY KEY, goal TEXT NOT NULL, status TEXT NOT NULL,
		created TEXT NOT NULL, updated TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO tasks (id, goal, status, created, updated) VALUES ('old', 'legacy goal', 'interrupted', '2026-01-01', '2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Open through the store: the migration must add detail without losing the row.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open of legacy DB (migration): %v", err)
	}
	got, err := s.GetTask(ctx, "old")
	if err != nil {
		t.Fatalf("legacy row lost after migration: %v", err)
	}
	if got.Goal != "legacy goal" || got.Status != "interrupted" {
		t.Errorf("legacy row mangled: %+v", got)
	}
	if got.Detail != "" {
		t.Errorf("migrated row detail = %q, want empty default", got.Detail)
	}
	// The migrated column is writable.
	if err := s.UpsertTask(ctx, Task{ID: "old", Goal: "legacy goal", Status: "running", Detail: `{"tip_sha":"z"}`}); err != nil {
		t.Fatalf("write to migrated column: %v", err)
	}
	if g, _ := s.GetTask(ctx, "old"); g.Detail != `{"tip_sha":"z"}` {
		t.Errorf("migrated detail = %q", g.Detail)
	}
	s.Close()

	// Re-Open must be a no-op migration (the ALTER is guarded): no error, data intact.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open after migration (idempotency): %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if g, _ := s2.GetTask(ctx, "old"); g.Detail != `{"tip_sha":"z"}` {
		t.Errorf("after re-Open detail = %q, want preserved", g.Detail)
	}
}
