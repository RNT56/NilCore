package agent_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/store"
)

func newCheckpoint(t *testing.T) (*agent.Checkpoint, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return agent.NewCheckpoint(s), s
}

func TestSIGTERMCheckpointAndResume(t *testing.T) {
	cp, s := newCheckpoint(t)
	ctx := context.Background()

	// A task begins...
	if err := cp.Begin(ctx, backend.Task{ID: "t1", Goal: "fix the bug"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Status != "running" {
		t.Fatalf("status = %q, want running", got.Status)
	}

	// SIGTERM: checkpoint cleanly (no partial state).
	if err := cp.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Status != "interrupted" {
		t.Fatalf("after interrupt status = %q, want interrupted", got.Status)
	}

	// Restart: the in-flight task is resumed from its checkpoint.
	inflight, _ := cp.InFlight(ctx)
	if len(inflight) != 1 || inflight[0].ID != "t1" {
		t.Fatalf("InFlight = %+v, want [t1]", inflight)
	}
	var resumed string
	err := cp.Resume(ctx, func(_ context.Context, tk backend.Task) (bool, error) {
		resumed = tk.ID
		return true, nil // succeeds on resume
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed != "t1" {
		t.Errorf("resumed %q, want t1", resumed)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Status != "done" {
		t.Errorf("after resume status = %q, want done", got.Status)
	}
}

func TestResumeFailsCleanly(t *testing.T) {
	cp, s := newCheckpoint(t)
	ctx := context.Background()
	_ = cp.Begin(ctx, backend.Task{ID: "t2", Goal: "x"})

	err := cp.Resume(ctx, func(context.Context, backend.Task) (bool, error) {
		return false, errors.New("backend exploded") // resume fails
	})
	if err != nil {
		t.Fatalf("Resume should not propagate per-task errors: %v", err)
	}
	if got, _ := s.GetTask(ctx, "t2"); got.Status != "failed" {
		t.Errorf("a task that can't resume must be failed cleanly; status=%q", got.Status)
	}
}
