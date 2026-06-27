package main

import (
	"context"
	"path/filepath"
	"testing"

	"nilcore/internal/store"
)

// TestObjectiveCRUDRoundTrip exercises the operator-only `nilcore objective` write
// path against a real store: add → list → disable (pause, not delete) → enable.
func TestObjectiveCRUDRoundTrip(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	runObjectiveAdd(ctx, s, []string{"-id", "keep-ci", "-goal", "keep CI green", "-priority", "5", "-period", "24h"})
	objs, err := s.ListObjectives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].ID != "keep-ci" || !objs[0].Enabled || objs[0].Priority != 5 || objs[0].Goal != "keep CI green" {
		t.Fatalf("add did not persist the objective faithfully: %+v", objs)
	}

	// Disable pauses (retains) the objective.
	runObjectiveSetEnabled(ctx, s, []string{"keep-ci"}, false)
	o, err := s.GetObjective(ctx, "keep-ci")
	if err != nil {
		t.Fatal(err)
	}
	if o.Enabled {
		t.Fatal("disable must pause the objective (not delete it)")
	}

	// Enable un-pauses it (read-modify-write through the typed store).
	runObjectiveSetEnabled(ctx, s, []string{"keep-ci"}, true)
	o, err = s.GetObjective(ctx, "keep-ci")
	if err != nil {
		t.Fatal(err)
	}
	if !o.Enabled {
		t.Fatal("enable must un-pause the objective")
	}
}
