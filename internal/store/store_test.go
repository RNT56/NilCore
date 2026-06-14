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
