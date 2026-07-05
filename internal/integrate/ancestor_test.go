package integrate

import (
	"context"
	"testing"
)

// TestMergeAncestorBranchIsIdempotentSuccess is the Fix #7 acceptance: a branch
// whose tip is already an ANCESTOR of the integration tip (its work is already on
// the tip) must fold as Merged && Verified with the tip UNCHANGED — an idempotent
// no-op — never a conflict and never a rollback. This models a verifier-green shard
// that committed nothing new (a DAG dependent cut from its dep's branch whose goal
// was already satisfied, or an artifact-only change under the gitignored
// .nilcore/): its surfaced branch is an ancestor of the merged tip, and a real
// `git merge --no-ff` of it would report "Already up to date" and then fail the
// commit ("nothing to commit"). Pre-fix that path burned rebuild attempts and exited
// `unmerged`; post-fix it is a clean success.
func TestMergeAncestorBranchIsIdempotentSuccess(t *testing.T) {
	repo := baseRepo(t)

	// t1 carries a real change; the integration tip will be main + t1 merged.
	branchFrom(t, repo, "task/t1", map[string]string{"t1.txt": "from t1\n"})
	// t0 is the ANCESTOR case: a branch pointing at base HEAD (main), which the tip
	// already contains. We model it as a branch with no new commit past main.
	hgit(t, repo, "branch", "task/t0", "main")

	log, read := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		NewEnv:   newEnvFor("README", func(string) bool { return true }),
		Log:      log,
	}

	// Fold t1 first (a real merge → new tip), then t0 (an ancestor of that tip).
	wt, results, err := it.Integrate(context.Background(),
		[]MergeItem{{ID: "t1", Branch: "task/t1"}, {ID: "t0", Branch: "task/t0"}})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	tipAfterT1 := results[0].SHA
	if !results[0].Merged || !results[0].Verified {
		t.Fatalf("t1 should merge green: %+v", results[0])
	}

	// The ancestor fold: SUCCESS, tip unchanged, NOT a conflict, NOT escalated.
	r0 := results[1]
	if !r0.Merged || !r0.Verified {
		t.Errorf("ancestor branch t0 should fold as Merged && Verified (no-op), got %+v", r0)
	}
	if r0.Conflict || r0.Escalate {
		t.Errorf("ancestor branch t0 must not conflict/escalate, got %+v", r0)
	}
	if r0.Err != nil {
		t.Errorf("ancestor branch t0 must have no error, got %v", r0.Err)
	}
	// The tip did not advance: the no-op merge leaves it at t1's merge SHA.
	if r0.SHA != tipAfterT1 || r0.PreSHA != tipAfterT1 {
		t.Errorf("ancestor fold changed the tip: SHA=%q PreSHA=%q, want both %q", r0.SHA, r0.PreSHA, tipAfterT1)
	}
	// The integration worktree tip is unchanged (still t1's merge).
	if h, _ := wt.Head(context.Background()); h != tipAfterT1 {
		t.Errorf("worktree tip = %q, want unchanged %q after the no-op ancestor fold", h, tipAfterT1)
	}
	// The audit trail records the no-op as a merge (with noop:true), never a conflict.
	if hasKind(read(), "integration_conflict") {
		t.Errorf("ancestor no-op emitted an integration_conflict event: %v", kinds(read()))
	}
}

// TestBranchContainedAncestorCheck is a focused guard on the ancestor predicate:
// a branch that is an ancestor of the tip reads contained; one that is not does not.
func TestBranchContainedAncestorCheck(t *testing.T) {
	repo := baseRepo(t)
	head := baseHead(t, repo)
	branchFrom(t, repo, "task/ahead", map[string]string{"ahead.txt": "x\n"})

	it := &Integrator{BaseRepo: repo, Log: nil}
	ctx := context.Background()
	// baseHead's own branch (main) is trivially an ancestor of head.
	if !it.branchContained(ctx, repo, "main", head) {
		t.Errorf("main should be contained in head %q", head)
	}
	// A branch AHEAD of head (a real new commit) is NOT an ancestor of head.
	if it.branchContained(ctx, repo, "task/ahead", head) {
		t.Errorf("task/ahead is ahead of %q and must not be contained", head)
	}
	// Empty inputs are never contained (defensive).
	if it.branchContained(ctx, repo, "", head) || it.branchContained(ctx, repo, "main", "") {
		t.Errorf("empty branch/tip must not be contained")
	}
}
