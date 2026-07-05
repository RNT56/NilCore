package swarm

import (
	"context"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/requeue"
)

// TestControllerPersistsLedgerAfterBump is the Fix #12 acceptance: the per-pass
// SaveState runs BEFORE bumpAndSelect spends the pass's red-claim attempts, so without
// an extra persist the bump would only reach disk on the next pass's boundary — a crash
// (or an exit at the passes/exhausted rail, which return without a further SaveState)
// in between would resume having FORGOTTEN the attempt, granting an extra retry. This
// test runs a single bounded pass over a still-red shard and asserts the DURABLE
// SwarmState.Ledger reflects the attempt the bump just spent.
func TestControllerPersistsLedgerAfterBump(t *testing.T) {
	h := newHarness(t)
	// The shard is red on pass 1 and stays red; MaxPasses:1 stops the run at the passes
	// rail right after the pass-1 bump — the exact exit that skips the next-boundary save.
	fn := h.fnFromStatusPlan(map[string][]artifact.Status{
		"swarm-run1-0": {artifact.StatusFail},
	})
	c := &Controller{
		Runner:   &Runner{Concurrency: 1, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{MaxPasses: 1}, // bounded: exits at the passes rail after pass 1
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}},
		shardSet("swarm-run1-0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Done {
		t.Fatalf("out = %+v, want not Done (the shard stayed red)", out)
	}

	// The persisted run state must carry the spent attempt: the harness writes a single
	// claim "c1", so the red Unit is keyed "swarm-run1-0/c1" and its attempt count is 1.
	st, lerr := h.q.LoadState(context.Background())
	if lerr != nil {
		t.Fatalf("LoadState: %v", lerr)
	}
	got := st.Ledger.Attempts["swarm-run1-0/c1"]
	if got != 1 {
		t.Errorf("persisted Ledger attempt for swarm-run1-0/c1 = %d, want 1 (the pass-1 bump must be durable)", got)
	}
}
