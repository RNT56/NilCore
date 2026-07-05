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

// TestOpenAppliesDurabilityPragmas proves Open hardens the connection: WAL mode
// (crash-safe + concurrent reads) and synchronous=NORMAL (fsync at checkpoints).
func TestOpenAppliesDurabilityPragmas(t *testing.T) {
	s := openTemp(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var sync int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 { // 1 == NORMAL
		t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
	}
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

// TestMemoryUpsertReplaces proves PutMemory is keyed by (scope, project, mkey): a
// second write for the same logical key REPLACES the value in place (no stale
// duplicate row) and returns the same row id.
func TestMemoryUpsertReplaces(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id1, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "task:1", Value: "old"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "task:1", Value: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("upsert kept the row: ids %d vs %d", id1, id2)
	}
	got, err := s.QueryMemory(ctx, "project", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != "new" {
		t.Fatalf("upsert must replace, got %+v", got)
	}
	// A different key under the same scope/project is independent.
	if _, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "task:2", Value: "other"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.QueryMemory(ctx, "project", "p"); len(g) != 2 {
		t.Errorf("distinct key must add a row, got %d", len(g))
	}
}

// TestPutMemoryUpdateReturnsCorrectID is the stale-rowid regression: on the ON
// CONFLICT UPDATE branch no new row is inserted, so LastInsertId would return the
// connection's LAST insert rowid — an UNRELATED row if some other insert ran in
// between. RETURNING id must instead give the id of the logical row we upserted. We
// force the hazard: create key A, then insert an unrelated event/memory row (bumping
// the connection's last-insert rowid), then UPDATE key A and confirm the returned id
// is still A's own row id.
func TestPutMemoryUpdateReturnsCorrectID(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	idA, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "A", Value: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated insert on the SAME shared connection advances last_insert_rowid to a
	// row that is NOT key A — the exact condition under which the old LastInsertId path
	// would have returned the wrong id on the update below.
	if _, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "B", Value: "b1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertEvent(ctx, Event{Time: time.Now(), Task: "t", Kind: "noise"}); err != nil {
		t.Fatal(err)
	}

	// Now UPDATE key A (ON CONFLICT branch). The returned id must be A's own id.
	idA2, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "A", Value: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if idA2 != idA {
		t.Fatalf("update returned a stale/unrelated id: got %d, want A's id %d", idA2, idA)
	}
	// And the value actually replaced in A's row.
	got, err := s.QueryMemory(ctx, "project", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.Key == "A" {
			if m.ID != idA {
				t.Errorf("A row id = %d, want %d", m.ID, idA)
			}
			if m.Value != "a2" {
				t.Errorf("A value = %q, want a2", m.Value)
			}
		}
	}
}

// TestEventSeqPersisted proves the mirror now carries the log Seq anchor: an event
// inserted with a Seq reads back with that same Seq, and EventsByTask orders by Seq
// (not just insertion id), so the SQLite backing can reproduce ordering / detect gaps.
func TestEventSeqPersisted(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Insert out of Seq order to prove ordering comes from Seq, not insertion id.
	for _, seq := range []uint64{2, 0, 1} {
		if err := s.InsertEvent(ctx, Event{
			Seq: seq, Time: time.Now(), Task: "t", Kind: "step",
			Detail: `{}`, Hash: "h", Prev: "p",
		}); err != nil {
			t.Fatalf("insert seq %d: %v", seq, err)
		}
	}
	evs, err := s.EventsByTask(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	for i, e := range evs {
		if e.Seq != uint64(i) {
			t.Errorf("event[%d].Seq = %d, want %d (ordered by log Seq, not insertion id)", i, e.Seq, i)
		}
	}
}

// TestEventSeqLegacyDBSentinel proves an events row written before the seq column
// existed migrates cleanly and reads back as the -1 sentinel → Seq 0-with-unknown,
// surfaced as Seq==0 here but never mistaken for a real anchor by a gap check (the
// column default is -1). We create a legacy events table, insert a row, then Open
// (which adds the column) and confirm the read does not error.
func TestEventSeqLegacyDBSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy_events.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-seq events table (no seq column), matching the historical schema.
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, task TEXT,
		kind TEXT NOT NULL, backend TEXT, detail TEXT, prev TEXT, hash TEXT)`); err != nil {
		t.Fatal(err)
	}
	// Populate the non-null-in-practice columns the way a real InsertEvent does (the
	// mirror never writes SQL NULLs), so the read-back exercises the seq migration, not
	// a NULL-scan artifact.
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO events (ts, task, kind, backend, detail, prev, hash) VALUES (?, 't', 'legacy', '', '', '', '')`,
		time.Now().UTC().Format(tsFmt)); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path) // migrateEventSeq adds the seq column with DEFAULT -1
	if err != nil {
		t.Fatalf("Open of legacy events DB (seq migration): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	evs, err := s.EventsByTask(ctx, "t")
	if err != nil {
		t.Fatalf("EventsByTask over migrated legacy DB: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != "legacy" {
		t.Fatalf("legacy row lost in migration: %+v", evs)
	}
	// The -1 sentinel reads back as Seq 0 (unknown anchor), never a spurious position.
	if evs[0].Seq != 0 {
		t.Errorf("legacy row Seq = %d, want 0 (unknown-anchor sentinel maps to 0)", evs[0].Seq)
	}
}

// TestMemoryUniqueMigration is the legacy-DB path: a memory table created WITHOUT the
// UNIQUE(scope,project,mkey) constraint, holding duplicate-key rows, must on Open be
// rebuilt to carry the constraint and collapse duplicates to the newest row per key —
// without erroring on a re-Open (idempotent).
func TestMemoryUniqueMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy_mem.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-constraint memory table: no UNIQUE.
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE memory (
		id INTEGER PRIMARY KEY AUTOINCREMENT, scope TEXT NOT NULL,
		project TEXT NOT NULL DEFAULT '', mkey TEXT NOT NULL,
		mvalue TEXT NOT NULL, created TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Two rows for the SAME logical key (the stale-duplicate bug) plus one distinct key.
	for _, row := range []struct{ k, v, c string }{
		{"task:1", "stale", "2026-01-01T00:00:00Z"},
		{"task:1", "fresh", "2026-01-02T00:00:00Z"},
		{"task:2", "other", "2026-01-01T00:00:00Z"},
	} {
		if _, err := legacy.ExecContext(ctx,
			`INSERT INTO memory (scope, project, mkey, mvalue, created) VALUES ('project','p',?,?,?)`,
			row.k, row.v, row.c); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open of legacy memory DB (migration): %v", err)
	}
	got, err := s.QueryMemory(ctx, "project", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("migration must collapse duplicate keys, got %d rows: %+v", len(got), got)
	}
	// The surviving task:1 row keeps the NEWEST value (max id wins).
	var task1 string
	for _, g := range got {
		if g.Key == "task:1" {
			task1 = g.Value
		}
	}
	if task1 != "fresh" {
		t.Errorf("migration kept stale value, task:1 = %q want fresh", task1)
	}
	// Post-migration the constraint is live: a same-key write replaces.
	if _, err := s.PutMemory(ctx, Memory{Scope: "project", Project: "p", Key: "task:1", Value: "newest"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.QueryMemory(ctx, "project", "p"); len(g) != 2 {
		t.Errorf("post-migration upsert must not add a row, got %d", len(g))
	}
	s.Close()

	// Re-Open is a no-op migration (already at current schema).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open after memory migration (idempotency): %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if g, _ := s2.QueryMemory(ctx, "project", "p"); len(g) != 2 {
		t.Errorf("after re-Open memory rows = %d, want 2 preserved", len(g))
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
