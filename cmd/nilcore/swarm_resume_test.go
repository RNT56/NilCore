package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"nilcore/internal/store"
	"nilcore/internal/swarm"
)

// resumeQueue opens a real SQLite-backed swarm Queue in a temp dir.
func resumeQueue(t *testing.T, runID string) (*swarm.Queue, func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "nilcore.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	q := swarm.NewQueue(st, nil, runID)
	return q, func() { _ = st.Close() }
}

// shardID builds an id inside this run's queue namespace ("swarm-<runID>-<n>"),
// which is the prefix ShardsByRun filters on.
func shardID(runID string, n int) string { return fmt.Sprintf("swarm-%s-%d", runID, n) }

func markShard(t *testing.T, q *swarm.Queue, id string, state swarm.ShardState) {
	t.Helper()
	if err := q.Mark(context.Background(), swarm.Shard{ID: id, State: state}); err != nil {
		t.Fatalf("mark %s: %v", id, err)
	}
}

// THE skipped-dependent false-green regression. A DAG dependent whose dep failed is
// durably Marked ShardSkipped, which persists as StatusQueued — a status queue.Failed
// never reads. If resume does not re-seed it, the Controller rebuilds `planned` from a
// seed set that omits it, termination honesty cannot see it, and the run converges
// exit 0 having never executed planned work (I2).
func TestResumeReseedsSkippedDependent(t *testing.T) {
	const runID = "run-x"
	q, done := resumeQueue(t, runID)
	defer done()

	ctx := context.Background()
	markShard(t, q, shardID(runID, 0), swarm.ShardFailed)  // A: the failed dep
	markShard(t, q, shardID(runID, 1), swarm.ShardSkipped) // B: dependent, never ran

	resumed := &swarm.SwarmState{RunID: runID}
	got, err := resumeIncomplete(ctx, q, resumed)
	if err != nil {
		t.Fatalf("resumeIncomplete: %v", err)
	}

	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
		if s.State != swarm.ShardQueued {
			t.Errorf("re-seeded shard %s has state %v, want ShardQueued (must dispatch afresh)", s.ID, s.State)
		}
	}
	if !ids[shardID(runID, 1)] {
		t.Fatal("the skipped dependent was NOT re-seeded — a resumed run would converge exit 0 without ever running it (I2)")
	}
	if ids[shardID(runID, 0)] {
		t.Error("the failed shard must be seeded by queue.Failed under the retry-budget gate, not here (it would get a free budget)")
	}
}

// Queued and Running shards were likewise never returned by either resume source.
func TestResumeReseedsQueuedAndRunning(t *testing.T) {
	const runID = "run-y"
	q, done := resumeQueue(t, runID)
	defer done()

	markShard(t, q, shardID(runID, 0), swarm.ShardQueued)  // never dispatched
	markShard(t, q, shardID(runID, 1), swarm.ShardRunning) // interrupted mid-flight

	got, err := resumeIncomplete(context.Background(), q, &swarm.SwarmState{RunID: runID})
	if err != nil {
		t.Fatalf("resumeIncomplete: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("re-seeded %d shards, want 2 (queued + running are unfinished planned work)", len(got))
	}
}

// A passed-but-unmerged shard is re-folded; a passed-and-merged one is already on the
// tip and must not be re-run (the pre-existing Fix #10 behavior must survive).
func TestResumeReseedsPassedUnmergedOnly(t *testing.T) {
	const runID = "run-z"
	q, done := resumeQueue(t, runID)
	defer done()

	markShard(t, q, shardID(runID, 0), swarm.ShardPassed) // merged
	markShard(t, q, shardID(runID, 1), swarm.ShardPassed) // verified, never folded

	resumed := &swarm.SwarmState{RunID: runID, Merged: []string{shardID(runID, 0)}}
	got, err := resumeIncomplete(context.Background(), q, resumed)
	if err != nil {
		t.Fatalf("resumeIncomplete: %v", err)
	}
	if len(got) != 1 || got[0].ID != shardID(runID, 1) {
		t.Fatalf("want only the passed-but-unmerged shard re-seeded, got %+v", got)
	}
}

func TestDedupeShardsKeepsFirstOccurrence(t *testing.T) {
	in := []swarm.Shard{
		{ID: "a", Attempt: 1},
		{ID: "b"},
		{ID: "a", Attempt: 9}, // a duplicate would dispatch twice and double-count the board
	}
	got := dedupeShards(in)
	if len(got) != 2 {
		t.Fatalf("dedupeShards returned %d shards, want 2", len(got))
	}
	if got[0].ID != "a" || got[0].Attempt != 1 {
		t.Errorf("dedupe must keep the first occurrence, got %+v", got[0])
	}
	if got[1].ID != "b" {
		t.Errorf("got[1] = %+v, want b", got[1])
	}
}
