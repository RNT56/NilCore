package swarm

import (
	"context"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/requeue"
)

// TestControllerSkippedDependentReRunsWhenDepGreens is the Fix #21 acceptance (I2 —
// no false green over a DAG). Shards A←B: B depends on A. A is RED on pass 1, so the
// DAG scheduler SKIPS B (it never runs, writes no artifact, and is invisible to
// requeue.Scan). A greens on pass 2. The run must:
//
//  1. NOT run B on pass 1 (skipped behind its red dep);
//  2. RE-RUN B on pass 2 once A is green (the dependent re-inclusion);
//  3. converge Done=true only AFTER B has actually run and merged — never a
//     Done=true/Remaining=0 while B's planned work was silently dropped.
func TestControllerSkippedDependentReRunsWhenDepGreens(t *testing.T) {
	h := newHarness(t)
	// A: red on pass 1, green on pass 2. B: green whenever it runs.
	plan := map[string]*script{
		"swarm-run1-0": {
			status: []artifact.Status{artifact.StatusFail, artifact.StatusPass},
			branch: []string{"", "task/swarm-run1-0"},
		},
		"swarm-run1-1": {branch: []string{"task/swarm-run1-1"}},
	}
	cf := newCaptureFn(h, plan)
	is := &integrateScript{}
	c := &Controller{
		Runner:    &Runner{Concurrency: 2, Fn: cf.fn},
		Queue:     h.q,
		Worktree:  h.worktree,
		Policy:    PassPolicy{UntilClean: true, MaxPasses: 6},
		Integrate: is.fn,
	}
	shards := []Shard{
		{ID: "swarm-run1-0", Goal: "build A", Kind: artifact.KindReport, State: ShardQueued},
		{ID: "swarm-run1-1", Goal: "build B on A", Kind: artifact.KindReport, State: ShardQueued,
			Deps: []string{"swarm-run1-0"}},
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// (1) B was skipped on pass 1: it ran EXACTLY once, on pass 2 (after A greened).
	if got := cf.calls("swarm-run1-1"); got != 1 {
		t.Errorf("dependent B ran %d times, want exactly 1 (skipped on pass 1, run on pass 2)", got)
	}
	// (3) The run converged green ONLY after B ran and merged — no false green.
	if !out.Done || out.Reason != ReasonConverged {
		t.Fatalf("out = %+v, want Done converged (B's planned work merged)", out)
	}
	if out.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0 (all planned work resolved)", out.Remaining)
	}
	// B's branch reached the integrated tip (its planned work is on the tip).
	last := is.orders[len(is.orders)-1]
	foundB := false
	for _, it := range last {
		if it.ID == "swarm-run1-1" {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("dependent B never folded into the tip; final fold = %+v", last)
	}
	// (2) B genuinely executed (its Fn was invoked) rather than being written off as a
	// verdict without running — the whole point of the re-inclusion. Its first-and-only
	// run is its FIRST attempt (Attempt 0 is correct: it was skipped, never attempted,
	// on pass 1), and it was cut from a base honoring its now-green dependency.
	bRun := cf.shard(t, "swarm-run1-1", 0)
	if bRun.State != ShardQueued {
		t.Errorf("dependent B dispatched in state %q, want queued", bRun.State)
	}
	if bRun.BaseRef != "task/swarm-run1-0" {
		t.Errorf("dependent B BaseRef = %q, want its now-green dependency's branch task/swarm-run1-0", bRun.BaseRef)
	}
}

// TestControllerSkippedDependentSurfacesWhenDepNeverGreens is the honesty backstop:
// if A can NEVER green (its retry budget is spent), B stays skipped/unresolved — the
// run must exit RED (Done=false, B counted in Remaining), never a false Done=true just
// because Scan sees no red Unit for the invisible skipped dependent (I2).
func TestControllerSkippedDependentSurfacesWhenDepNeverGreens(t *testing.T) {
	h := newHarness(t)
	// A is red forever; with MaxAttempts=1 it exhausts after pass 1 with no green.
	plan := map[string]*script{
		"swarm-run1-0": {status: []artifact.Status{artifact.StatusFail}},
		"swarm-run1-1": {branch: []string{"task/swarm-run1-1"}},
	}
	cf := newCaptureFn(h, plan)
	is := &integrateScript{}
	c := &Controller{
		Runner:    &Runner{Concurrency: 2, Fn: cf.fn},
		Queue:     h.q,
		Worktree:  h.worktree,
		Policy:    PassPolicy{UntilClean: true, MaxPasses: 6},
		Integrate: is.fn,
	}
	shards := []Shard{
		{ID: "swarm-run1-0", Goal: "build A", Kind: artifact.KindReport, State: ShardQueued},
		{ID: "swarm-run1-1", Goal: "build B on A", Kind: artifact.KindReport, State: ShardQueued,
			Deps: []string{"swarm-run1-0"}},
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 1}}, shards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Done {
		t.Errorf("out = %+v, want Done=false (A never greened, B never ran — no false converge)", out)
	}
	if out.Remaining < 1 {
		t.Errorf("Remaining = %d, want >=1 (the never-run dependent B is counted, I2)", out.Remaining)
	}
	// B never ran — its planned work was NOT silently swallowed by a green verdict.
	if got := cf.calls("swarm-run1-1"); got != 0 {
		t.Errorf("dependent B ran %d times, want 0 (its dep never greened)", got)
	}
}
