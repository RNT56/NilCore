package store

// Internal-package test for the objectives-table success-cadence migration
// (migrateObjectiveCadence): a DB created before retry_period_ns / last_success
// existed is ALTERed to carry both columns, additively and idempotently, without
// losing an existing row. It calls the unexported migration directly (like the
// other guarded ALTERs) so the success-cadence persistence is proven even without
// exercising the whole Open path.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/objective"
)

// TestMigrateObjectiveCadenceAddsColumns stands up a legacy objectives table (the
// pre-retry-cadence shape: no retry_period_ns / last_success), then proves the
// migration adds both columns, preserves the legacy row, and leaves the columns
// writable/readable through the typed methods.
func TestMigrateObjectiveCadenceAddsColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy_obj.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE objectives (
		id TEXT PRIMARY KEY, goal TEXT NOT NULL DEFAULT '', priority INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1, min_period_ns INTEGER NOT NULL DEFAULT 0,
		last_run TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO objectives (id, goal, priority, enabled, min_period_ns, last_run)
		 VALUES ('old', 'keep CI green', 5, 1, ?, '')`, int64(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open as a *Store WITHOUT the full Open() migration pipeline, then run the
	// targeted migration directly so this test does not depend on migrate() wiring.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	t.Cleanup(func() { s.Close() })

	if err := s.migrateObjectiveCadence(ctx); err != nil {
		t.Fatalf("migrateObjectiveCadence: %v", err)
	}

	// The legacy row survives and its new columns default to zero.
	got, err := s.GetObjective(ctx, "old")
	if err != nil {
		t.Fatalf("legacy row lost after migration: %v", err)
	}
	if got.Goal != "keep CI green" || got.MinPeriod != 6*time.Hour {
		t.Errorf("legacy row mangled: %+v", got)
	}
	if got.RetryPeriod != 0 || !got.LastSuccess.IsZero() {
		t.Errorf("migrated defaults wrong: retry=%v last_success=%v", got.RetryPeriod, got.LastSuccess)
	}

	// The migrated columns are writable/readable through the typed methods.
	success := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	if err := s.PutObjective(ctx, objective.Objective{
		ID: "old", Goal: "keep CI green", Priority: 5, Enabled: true,
		MinPeriod: 6 * time.Hour, RetryPeriod: time.Hour, LastSuccess: success,
	}); err != nil {
		t.Fatalf("write migrated columns: %v", err)
	}
	got, _ = s.GetObjective(ctx, "old")
	if got.RetryPeriod != time.Hour || !got.LastSuccess.Equal(success) {
		t.Errorf("migrated columns not persisted: retry=%v last_success=%v", got.RetryPeriod, got.LastSuccess)
	}

	// Idempotent: a second run is a guarded no-op (columns already present).
	if err := s.migrateObjectiveCadence(ctx); err != nil {
		t.Fatalf("second migrateObjectiveCadence must be a no-op: %v", err)
	}
}
