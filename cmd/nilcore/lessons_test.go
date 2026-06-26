package main

import (
	"context"
	"path/filepath"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/memory"
	"nilcore/internal/store"
)

func lessonsFailEv(verifierID, failClass string) eventlog.Event {
	return eventlog.Event{Kind: "verify", Detail: map[string]any{
		"passed": false, "verifier_id": verifierID, "fail_class": failClass,
	}}
}

func TestWireLessonsFoldsScarsIntoMemory(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Kind: "task_start"})
	log.Append(lessonsFailEv("go-test", "compile"))
	log.Append(lessonsFailEv("go-test", "compile")) // recurs ⇒ a scar (>= MinRecurrence)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mem := memory.New(s)
	ctx := context.Background()

	// Default-off: NILCORE_LESSONS unset ⇒ nothing is folded (byte-identical).
	t.Setenv("NILCORE_LESSONS", "")
	wireLessons(logPath, mem)
	if recs, _ := mem.Query(ctx, memory.ScopeGlobal, "", ""); len(recs) != 0 {
		t.Fatalf("default-off must fold nothing, got %d record(s)", len(recs))
	}

	// Opted in: the recurring scar is folded into cross-project memory.
	t.Setenv("NILCORE_LESSONS", "1")
	wireLessons(logPath, mem)
	recs, _ := mem.Query(ctx, memory.ScopeGlobal, "", "")
	if len(recs) == 0 {
		t.Fatal("NILCORE_LESSONS on must fold the recurring scar into memory")
	}

	// Re-running is idempotent (memory.Remember dedupes), so scars don't pile up.
	wireLessons(logPath, mem)
	again, _ := mem.Query(ctx, memory.ScopeGlobal, "", "")
	if len(again) != len(recs) {
		t.Fatalf("re-fold must dedupe: had %d, now %d", len(recs), len(again))
	}

	// A nil memory is a no-op (no panic) — the persistence backbone may be absent.
	wireLessons(logPath, nil)
}
