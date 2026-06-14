package blackboard_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"nilcore/internal/blackboard"
	"nilcore/internal/store"
)

func TestConcurrentReadWrite(t *testing.T) {
	bb := blackboard.New(nil)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("task-%d", i)
			_ = bb.SetStatus(context.Background(), id, "g", "done")
			bb.PutArtifact(id, "result")
			_, _ = bb.Status(id)
			_ = bb.Snapshot()
		}(i)
	}
	wg.Wait()

	snap := bb.Snapshot()
	if len(snap) != n {
		t.Fatalf("snapshot has %d statuses, want %d", len(snap), n)
	}
	for id, st := range snap {
		if st != "done" {
			t.Fatalf("%s = %q, want done", id, st)
		}
	}
	if v, ok := bb.Artifact("task-0"); !ok || v != "result" {
		t.Errorf("artifact = %q, %v", v, ok)
	}
}

func TestStoreBacked(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "bb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bb := blackboard.New(s)
	ctx := context.Background()
	if err := bb.SetStatus(ctx, "t1", "fix bug", "running"); err != nil {
		t.Fatal(err)
	}
	if err := bb.SetStatus(ctx, "t1", "fix bug", "done"); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil || got.Status != "done" {
		t.Fatalf("persisted task = %+v, %v", got, err)
	}
}
