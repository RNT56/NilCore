package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/store"
)

func TestExperienceProjectionRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exp.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Existing queries are unaffected by the additive projection tables.
	if _, err := s.QueryMemory(ctx, "global", ""); err != nil {
		t.Fatalf("existing memory query broke with exp_* tables present: %v", err)
	}

	seen := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	if err := s.UpsertBackendStanding(ctx, store.BackendStanding{
		Class: "go-refactor", Backend: "native", Races: 3, Passes: 2, CostUSD: 0.5, LatencyNS: 1000, LastSeen: seen,
	}); err != nil {
		t.Fatalf("upsert backend: %v", err)
	}
	got, err := s.BackendStandings(ctx, "go-refactor")
	if err != nil || len(got) != 1 {
		t.Fatalf("backend standings = %v (err %v), want 1 row", got, err)
	}
	if g := got[0]; g.Races != 3 || g.Passes != 2 || g.CostUSD != 0.5 || g.LatencyNS != 1000 || !g.LastSeen.Equal(seen) {
		t.Errorf("backend standing round-trip mismatch: %+v", g)
	}
	if empty, _ := s.BackendStandings(ctx, "nope"); len(empty) != 0 {
		t.Errorf("unknown class should yield no rows, got %v", empty)
	}

	if err := s.UpsertConfigStanding(ctx, store.ConfigStanding{Config: "sonnet", PassRate: 0.9, TotalCost: 1.2, Cases: 10}); err != nil {
		t.Fatalf("upsert config: %v", err)
	}
	cs, _ := s.ConfigStandings(ctx)
	if len(cs) != 1 || cs[0].PassRate != 0.9 {
		t.Fatalf("config standings round-trip mismatch: %+v", cs)
	}

	// ExpMeta: fresh ⇒ ok=false; after Set ⇒ round-trips.
	if _, ok, _ := s.ExpMeta(ctx); ok {
		t.Errorf("fresh DB should report no projection watermark")
	}
	if err := s.SetExpMeta(ctx, store.ExpMeta{SourceSeq: 5, ChainOK: true, RebuiltAt: seen}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	m, ok, _ := s.ExpMeta(ctx)
	if !ok || m.SourceSeq != 5 || !m.ChainOK || !m.RebuiltAt.Equal(seen) {
		t.Errorf("meta round-trip mismatch: %+v ok=%v", m, ok)
	}

	// Reopen the same DB: the additive DDL is idempotent and the projection persists.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen (idempotent DDL): %v", err)
	}
	defer s2.Close()
	if again, _ := s2.BackendStandings(ctx, "go-refactor"); len(again) != 1 {
		t.Errorf("standing did not persist across reopen: %v", again)
	}
}
