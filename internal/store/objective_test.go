package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/objective"
	"nilcore/internal/store"
)

// TestObjectiveRoundTrip proves a standing objective round-trips through the typed
// *Store methods with every field preserved (enabled flag, MinPeriod/RetryPeriod as
// Durations, LastRun/LastSuccess as UTC instants), and that PutObjective is an upsert
// by ID.
func TestObjectiveRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "obj.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	last := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	success := time.Date(2026, 6, 26, 8, 30, 0, 0, time.UTC)
	want := objective.Objective{
		ID:          "keep-ci-green",
		Goal:        "keep CI green",
		Priority:    10,
		Enabled:     true,
		MinPeriod:   6 * time.Hour,
		RetryPeriod: 30 * time.Minute,
		LastRun:     last,
		LastSuccess: success,
	}
	if err := s.PutObjective(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetObjective(ctx, "keep-ci-green")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != want.ID || got.Goal != want.Goal || got.Priority != want.Priority ||
		got.Enabled != want.Enabled || got.MinPeriod != want.MinPeriod ||
		got.RetryPeriod != want.RetryPeriod || !got.LastRun.Equal(last) ||
		!got.LastSuccess.Equal(success) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// Upsert by ID replaces the whole record.
	want.Goal = "keep CI green and fast"
	want.Priority = 20
	want.Enabled = false
	if err := s.PutObjective(ctx, want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetObjective(ctx, "keep-ci-green")
	if got.Goal != "keep CI green and fast" || got.Priority != 20 || got.Enabled {
		t.Errorf("upsert did not replace: %+v", got)
	}

	all, _ := s.ListObjectives(ctx)
	if len(all) != 1 {
		t.Errorf("upsert created a duplicate row: %d objectives", len(all))
	}
}

// TestObjectiveZeroValueRoundTrip proves a never-run objective (zero LastRun) and a
// zero MinPeriod round-trip cleanly — the zero Time is stored as ” and parses back
// to a zero Time, never a spurious epoch.
func TestObjectiveZeroValueRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "obj.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.PutObjective(ctx, objective.Objective{ID: "fresh", Goal: "g", Enabled: true}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetObjective(ctx, "fresh")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LastRun.IsZero() {
		t.Errorf("never-run LastRun = %v, want zero", got.LastRun)
	}
	if got.MinPeriod != 0 {
		t.Errorf("zero MinPeriod = %v, want 0", got.MinPeriod)
	}
}

// TestObjectiveGetMissing proves a missing ID yields objective.ErrNotFound (testable
// with errors.Is), never a bare sql.ErrNoRows — the contract the leaf's Backlog.Get
// relies on to normalize not-found.
func TestObjectiveGetMissing(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "obj.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	_, err = s.GetObjective(ctx, "nope")
	if !errors.Is(err, objective.ErrNotFound) {
		t.Errorf("get missing = %v, want objective.ErrNotFound", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Errorf("get missing leaked sql.ErrNoRows: %v", err)
	}
}

// TestObjectiveListByPriorityAndEnabled proves List returns every objective (enabled
// or not), ordered by descending priority then ascending ID, and that Disable pauses
// (rather than deletes) a row.
func TestObjectiveListByPriorityAndEnabled(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "obj.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	seed := []objective.Objective{
		{ID: "b-low", Goal: "g", Priority: 1, Enabled: true},
		{ID: "a-high", Goal: "g", Priority: 5, Enabled: false},
		{ID: "c-high", Goal: "g", Priority: 5, Enabled: true},
	}
	for _, o := range seed {
		if err := s.PutObjective(ctx, o); err != nil {
			t.Fatalf("put %s: %v", o.ID, err)
		}
	}

	all, err := s.ListObjectives(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Disabled rows are RETAINED (paused, not deleted): all three come back.
	if len(all) != 3 {
		t.Fatalf("list = %d objectives, want 3 (disabled retained)", len(all))
	}
	// Order: priority 5 before 1; ties (a-high, c-high) by ascending ID.
	wantOrder := []string{"a-high", "c-high", "b-low"}
	for i, id := range wantOrder {
		if all[i].ID != id {
			t.Errorf("list[%d] = %q, want %q (priority desc, id asc)", i, all[i].ID, id)
		}
	}

	// Disable pauses, never deletes.
	if err := s.DisableObjective(ctx, "c-high"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ := s.GetObjective(ctx, "c-high")
	if got.Enabled {
		t.Errorf("c-high still enabled after disable")
	}
	if all, _ := s.ListObjectives(ctx); len(all) != 3 {
		t.Errorf("disable deleted a row: %d objectives, want 3", len(all))
	}
	// Disabling an absent ID is a not-found, not a silent success.
	if err := s.DisableObjective(ctx, "ghost"); !errors.Is(err, objective.ErrNotFound) {
		t.Errorf("disable missing = %v, want objective.ErrNotFound", err)
	}
}

// TestObjectiveStoreSatisfiesLeafSeam proves *store.Store (via ObjectiveStore) is a
// drop-in objective.Store: an objective.Backlog built over it selects the highest-
// priority due objective end-to-end. This is the contract the wiring layer relies on.
func TestObjectiveStoreSatisfiesLeafSeam(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "obj.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	backlog := objective.New(s.ObjectiveStore())
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	// Two enabled-and-due objectives; the higher priority must win.
	if err := backlog.Put(ctx, objective.Objective{ID: "low", Goal: "g", Priority: 1, Enabled: true}); err != nil {
		t.Fatalf("put low: %v", err)
	}
	if err := backlog.Put(ctx, objective.Objective{ID: "high", Goal: "g", Priority: 9, Enabled: true}); err != nil {
		t.Fatalf("put high: %v", err)
	}
	// A disabled objective must never be selected.
	if err := backlog.Put(ctx, objective.Objective{ID: "off", Goal: "g", Priority: 99, Enabled: false}); err != nil {
		t.Fatalf("put off: %v", err)
	}

	sel, ok, err := backlog.NextIdle(ctx, now)
	if err != nil || !ok {
		t.Fatalf("NextIdle = ok %v, err %v, want a selection", ok, err)
	}
	if sel.ID != "high" {
		t.Errorf("NextIdle selected %q, want high", sel.ID)
	}

	// MarkAttempt advances LastRun through the store; with a MinPeriod it debounces.
	if err := backlog.Put(ctx, objective.Objective{ID: "high", Goal: "g", Priority: 9, Enabled: true, MinPeriod: time.Hour}); err != nil {
		t.Fatalf("put high w/ period: %v", err)
	}
	if err := backlog.MarkAttempt(ctx, "high", now); err != nil {
		t.Fatalf("mark attempt: %v", err)
	}
	got, _ := s.GetObjective(ctx, "high")
	if !got.LastRun.Equal(now) {
		t.Errorf("MarkAttempt did not persist LastRun: %v", got.LastRun)
	}
	// Now "high" is debounced; "low" (no period) becomes the selection.
	sel, ok, _ = backlog.NextIdle(ctx, now)
	if !ok || sel.ID != "low" {
		t.Errorf("after MarkAttempt, NextIdle = %q ok=%v, want low", sel.ID, ok)
	}
}

// TestObjectiveAdditiveGolden proves the objectives table is purely additive: a fresh
// DB Opens cleanly, the pre-existing events/memory/tasks queries are byte-identical
// with the new table present, and an empty backlog yields no rows (the default-off
// path). This is the golden guard that the nil/default path is unchanged.
func TestObjectiveAdditiveGolden(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "golden.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Existing surfaces are unaffected by the additive objectives table.
	if _, err := s.QueryMemory(ctx, "global", ""); err != nil {
		t.Fatalf("memory query broke with objectives table present: %v", err)
	}
	if err := s.UpsertTask(ctx, store.Task{ID: "t1", Goal: "g", Status: "running"}); err != nil {
		t.Fatalf("task upsert broke with objectives table present: %v", err)
	}
	if _, err := s.EventsByTask(ctx, "t1"); err != nil {
		t.Fatalf("events query broke with objectives table present: %v", err)
	}

	// Default-off: an empty backlog yields nothing and selects nothing.
	if all, err := s.ListObjectives(ctx); err != nil || len(all) != 0 {
		t.Errorf("empty objectives = %v (err %v), want nil", all, err)
	}
	if _, ok, err := objective.New(s.ObjectiveStore()).NextIdle(ctx, time.Now()); err != nil || ok {
		t.Errorf("empty backlog NextIdle = ok %v err %v, want no selection", ok, err)
	}
}

// TestObjectiveLegacyDBOpensClean proves the additive DDL is old-DB-safe: a DB created
// before the objectives table existed (the pre-AUTO-T01 schema) Opens cleanly, gains
// the table, and the objective CRUD works — with the pre-existing data intact.
func TestObjectiveLegacyDBOpensClean(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Stand up a legacy DB with only the original tasks table (no objectives table).
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE tasks (
		id TEXT PRIMARY KEY, goal TEXT NOT NULL, status TEXT NOT NULL,
		created TEXT NOT NULL, updated TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO tasks (id, goal, status, created, updated) VALUES ('old', 'legacy', 'interrupted', '2026-01-01', '2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Open through the store: the CREATE TABLE IF NOT EXISTS adds objectives cleanly.
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open legacy DB: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Pre-existing data is intact.
	if got, err := s.GetTask(ctx, "old"); err != nil || got.Goal != "legacy" {
		t.Fatalf("legacy row lost: %+v, %v", got, err)
	}
	// The new objective surface works on the migrated DB.
	if err := s.PutObjective(ctx, objective.Objective{ID: "o1", Goal: "g", Enabled: true}); err != nil {
		t.Fatalf("objective CRUD on legacy DB: %v", err)
	}
	if got, err := s.GetObjective(ctx, "o1"); err != nil || got.ID != "o1" {
		t.Fatalf("get objective on legacy DB: %+v, %v", got, err)
	}
}

// TestObjectiveIdempotentReopen proves the additive DDL is idempotent and objectives
// persist across a reopen — re-running Open never errors and never drops data.
func TestObjectiveIdempotentReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.PutObjective(ctx, objective.Objective{ID: "persist", Goal: "g", Priority: 3, Enabled: true, MinPeriod: time.Minute}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen (idempotent DDL): %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	got, err := s2.GetObjective(ctx, "persist")
	if err != nil {
		t.Fatalf("objective did not persist across reopen: %v", err)
	}
	if got.Priority != 3 || got.MinPeriod != time.Minute {
		t.Errorf("reopened objective mismatch: %+v", got)
	}
}
