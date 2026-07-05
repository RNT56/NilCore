package swarm

import (
	"context"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/requeue"
)

// TestConflictRebuildRedReRunDoesNotResurrectOldBranch is the Fix #9 acceptance. A
// shard greens on pass 1 but its branch CONFLICTS on merge, so it is marked
// conflictStale and requeued to rebuild. On pass 2 the rebuild re-runs RED (the
// verifier fails). The stale mark must SURVIVE that red re-run: passed[S] still holds
// the OLD green result with the rolled-back (conflicting) branch, so clearing the mark
// would resurrect that known-conflicting branch for a doomed re-merge that burns the
// merge Ledger. The fix deletes the stale mark only in the GREEN arm; this test proves
// the old branch is NEVER re-presented to the integrator after the red re-run.
func TestConflictRebuildRedReRunDoesNotResurrectOldBranch(t *testing.T) {
	h := newHarness(t)
	// S greens on pass 1 (branch b1), then RED on pass 2 (its rebuild fails), then
	// green again on pass 3 (branch b3). A companion clean shard keeps the run moving.
	plan := map[string]*script{
		"swarm-run1-0": {branch: []string{"task/s0"}}, // clean, merges pass 1
		"swarm-run1-1": {
			status: []artifact.Status{artifact.StatusPass, artifact.StatusFail, artifact.StatusPass},
			branch: []string{"task/s1-v1", "", "task/s1-v3"},
		},
	}
	cf := newCaptureFn(h, plan)
	// S conflicts on its FIRST fold attempt only, then would merge clean.
	is := &integrateScript{conflictOn: map[string]int{"swarm-run1-1": 1}}
	c := &Controller{
		Runner:    &Runner{Concurrency: 2, Fn: cf.fn},
		Queue:     h.q,
		Worktree:  h.worktree,
		Policy:    PassPolicy{UntilClean: true, MaxPasses: 8},
		Integrate: is.fn,
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 5}},
		shardSet("swarm-run1-0", "swarm-run1-1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Fatalf("out = %+v, want Done converged", out)
	}

	// The KEYSTONE: across ALL fold orders, the OLD conflicting branch (task/s1-v1) is
	// presented to the integrator EXACTLY ONCE — on the pass-1 fold that conflicted. It
	// must NEVER be re-folded on the red pass-2 re-run (that is the resurrection bug); the
	// only later fold of S carries the FRESH rebuilt branch (task/s1-v3).
	v1Folds, v3Folds := 0, 0
	for _, order := range is.orders {
		for _, it := range order {
			if it.ID != "swarm-run1-1" {
				continue
			}
			switch it.Branch {
			case "task/s1-v1":
				v1Folds++
			case "task/s1-v3":
				v3Folds++
			}
		}
	}
	if v1Folds != 1 {
		t.Errorf("old conflicting branch task/s1-v1 folded %d times, want exactly 1 (never resurrected)", v1Folds)
	}
	if v3Folds < 1 {
		t.Errorf("fresh rebuilt branch task/s1-v3 folded %d times, want >=1 (the real fix)", v3Folds)
	}
	// Both shards end merged (the run really converged, not just stopped).
	assertMerged(t, h, "swarm-run1-0", "swarm-run1-1")
}

// assertMerged fails unless every id is in the persisted SwarmState.Merged set.
func assertMerged(t *testing.T, h *harness, ids ...string) {
	t.Helper()
	st, err := h.q.LoadState(context.Background())
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := map[string]bool{}
	for _, id := range st.Merged {
		got[id] = true
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("shard %q not in persisted Merged set %v", id, st.Merged)
		}
	}
}
