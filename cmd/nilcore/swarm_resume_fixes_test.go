package main

// swarm_resume_fixes_test.go covers the resume/branch-collision correctness fixes in
// the `nilcore swarm` wiring: Fix #10 (a durably-passed-but-unfolded shard is re-seeded
// on resume, never dropped), Fix #11 (a resume-loaded pass-1 failure whose unsuffixed
// branch already exists takes a fresh suffix instead of failing the worktree cut), and
// Fix #13 (an explicit `--resume --retries N` overrides the persisted retry budget).

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/budget"
	"nilcore/internal/store"
	"nilcore/internal/swarm"
	"nilcore/internal/swarm/preset"
	"nilcore/internal/verify"
)

// TestSwarmResumeReSeedsPassedButUnfolded is the Fix #10 acceptance: a shard the
// interrupted process durably Marked StatusPassed whose branch was never folded (its id
// is NOT in the persisted SwarmState.Merged set) must be RE-SEEDED into the resumed
// run's initial set — as a fresh queued shard — so the Controller re-runs and re-folds
// it. Otherwise its verified work stays off the tip and the resume exits 0 having
// silently dropped it (I2). A passed shard already in Merged must NOT be re-seeded.
func TestSwarmResumeReSeedsPassedButUnfolded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	runID := "run-fix10"
	q := swarm.NewQueue(st, discardLog(t), runID)

	// The prior run merged shard 0 but crashed with shard 1 passed-yet-unfolded (its id
	// is absent from Merged). Shard 2 is a plain failure (queue.Failed already returns it).
	prior := swarm.SwarmState{RunID: runID, Goal: "g", Preset: "benchmark", Pass: 3,
		Ledger: requeueLedger(3), Merged: []string{"swarm-" + runID + "-0"}}
	if err := q.SaveState(ctx, prior); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	mark := func(id string, state swarm.ShardState, branch string) {
		s := swarm.Shard{ID: id, Goal: "g", Kind: artifact.KindBenchmark, Pack: "benchmark",
			Role: "implementer", State: state, Branch: branch}
		if err := q.Mark(ctx, s); err != nil {
			t.Fatalf("Mark %s: %v", id, err)
		}
	}
	mark("swarm-"+runID+"-0", swarm.ShardPassed, "swarm/swarm-"+runID+"-0") // passed AND merged
	mark("swarm-"+runID+"-1", swarm.ShardPassed, "swarm/swarm-"+runID+"-1") // passed, NOT merged
	mark("swarm-"+runID+"-2", swarm.ShardFailed, "")                        // plain failure

	shardFile := filepath.Join(t.TempDir(), "shards.txt")
	_ = writeFile(shardFile, "x\n")
	sf := parseSwarmFlags(t, "-preset", "benchmark", "-shard-file", shardFile, "-resume")
	asm, err := buildSwarm(swarmDeps{flags: sf, boot: testBoot(), log: discardLog(t), dir: newGoRepo(t),
		ledger: budget.New(), pool: testPool(t), store: st})
	if err != nil {
		t.Fatalf("buildSwarm resume: %v", err)
	}

	ids := map[string]bool{}
	for _, s := range asm.initial {
		ids[s.ID] = true
	}
	// The unfolded-passed shard 1 IS re-seeded (Fix #10); the plain failure 2 is present
	// (queue.Failed); the already-merged shard 0 is NOT re-seeded.
	if !ids["swarm-"+runID+"-1"] {
		t.Errorf("passed-but-unfolded shard was NOT re-seeded on resume; initial=%v", ids)
	}
	if !ids["swarm-"+runID+"-2"] {
		t.Errorf("plain-failed shard missing from resume initial; initial=%v", ids)
	}
	if ids["swarm-"+runID+"-0"] {
		t.Errorf("already-merged shard 0 was wrongly re-seeded; initial=%v", ids)
	}
	// The re-seeded shard is a fresh, dispatchable unit (queued, no stale branch/base).
	for _, s := range asm.initial {
		if s.ID == "swarm-"+runID+"-1" {
			if s.State != swarm.ShardQueued || s.Branch != "" || s.BaseRef != "" {
				t.Errorf("re-seeded shard not reset: %+v", s)
			}
		}
	}
}

// TestSwarmResumeExplicitRetriesOverrides is the Fix #13 acceptance: `--resume` adopts
// the persisted SwarmState wholesale, so an EXPLICIT `--retries N` on the resume command
// must override the persisted Ledger.MaxAttempts (the operator asked for a new ceiling);
// WITHOUT an explicit --retries, the persisted value stands.
func TestSwarmResumeExplicitRetriesOverrides(t *testing.T) {
	// seed persists a prior run with a retry budget of 2 into a fresh store and returns
	// the store + a shard file, so each sub-case resumes an independent interrupted run.
	seed := func(t *testing.T) (*store.Store, string) {
		t.Helper()
		st := testStore(t)
		runID := "run-fix13"
		q := swarm.NewQueue(st, discardLog(t), runID)
		prior := swarm.SwarmState{RunID: runID, Goal: "g", Preset: "benchmark", Pass: 1,
			Ledger: requeueLedger(2)} // persisted budget = 2
		if err := q.SaveState(context.Background(), prior); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		shardFile := filepath.Join(t.TempDir(), "shards.txt")
		_ = writeFile(shardFile, "x\n")
		return st, shardFile
	}
	build := func(t *testing.T, st *store.Store, shardFile string, explicit map[string]bool, args ...string) swarm.SwarmState {
		t.Helper()
		sf := parseSwarmFlags(t, args...)
		asm, err := buildSwarm(swarmDeps{flags: sf, boot: testBoot(), log: discardLog(t), dir: newGoRepo(t),
			ledger: budget.New(), pool: testPool(t), store: st, explicitFlags: explicit})
		if err != nil {
			t.Fatalf("buildSwarm: %v", err)
		}
		return asm.state
	}

	// (a) Explicit --retries 5 overrides the persisted 2.
	st, shardFile := seed(t)
	got := build(t, st, shardFile, map[string]bool{"retries": true},
		"-preset", "benchmark", "-shard-file", shardFile, "-resume", "-retries", "5")
	if got.Ledger.MaxAttempts != 5 {
		t.Errorf("explicit --retries: MaxAttempts = %d, want 5 (override the persisted 2)", got.Ledger.MaxAttempts)
	}

	// (b) No explicit --retries: the persisted 2 stands (the default flag value is ignored).
	st2, shardFile2 := seed(t)
	got2 := build(t, st2, shardFile2, map[string]bool{}, // nothing explicitly set
		"-preset", "benchmark", "-shard-file", shardFile2, "-resume")
	if got2.Ledger.MaxAttempts != 2 {
		t.Errorf("no explicit --retries: MaxAttempts = %d, want the persisted 2", got2.Ledger.MaxAttempts)
	}
}

// TestShardRunSuffixesWhenBranchExists is the Fix #11 acceptance: a resume-loaded pass-1
// failure persists Attempt=0 and BaseRef="", but its unsuffixed branch swarm/<leaf>
// ALREADY EXISTS (Release keeps branches). The old guard (Attempt>0 || BaseRef!="")
// missed it, so `git worktree add -b swarm/<leaf>` failed deterministically at the cut
// and the shard recordFail'd without running. With the existence check, the shard takes
// a fresh suffix and proceeds to run (here, to the nil-box fail-closed gate — proving the
// worktree cut SUCCEEDED rather than colliding).
func TestShardRunSuffixesWhenBranchExists(t *testing.T) {
	pre, _, err := preset.Resolve("ui")
	if err != nil {
		t.Fatalf("resolve ui: %v", err)
	}
	repo := newGoRepo(t)
	id := "swarm-fix11-0"
	// Pre-create the exact unsuffixed branch a resume-loaded pass-1 failure would target,
	// modeling the prior process's Release-kept branch.
	existing := "swarm/" + leafName(id)
	if out, err := exec.Command("git", "-C", repo, "branch", existing).CombinedOutput(); err != nil {
		t.Fatalf("git branch %s: %v (%s)", existing, err, out)
	}

	sc := &shardContext{
		deps:        swarmDeps{boot: testBoot(), log: discardLog(t)},
		preset:      pre,
		packName:    pre.VerifyPacks[0],
		deliverable: deliverableSet{kind: pre.Kind},
		pool:        testPool(t),
		ledger:      budget.New(),
		board:       newTestBoard(),
		repo:        repo,
		collateRoot: repo,
		newEnv:      func(string) buildEnv { return buildEnv{Box: nil, Verifier: verify.Pass{}} },
	}

	// A resume-loaded pass-1 failure: Attempt=0, BaseRef="" — the case the old guard missed.
	res := sc.run(context.Background(), swarm.Shard{
		ID: id, Goal: "g", Kind: pre.Kind, Pack: pre.VerifyPacks[0],
		Role: string(pre.Role), State: swarm.ShardQueued,
	})
	// Pre-fix: the error names the worktree cut / branch collision. Post-fix: the cut
	// SUCCEEDS (a suffixed branch) and the run reaches the nil-box gate.
	if res.Err == nil {
		t.Fatal("nil-box shard must fail closed after running")
	}
	if strings.Contains(res.Err.Error(), "worktree") {
		t.Fatalf("worktree cut still collided on the existing branch (Fix #11 not applied): %v", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "no sandbox") {
		t.Fatalf("err = %v, want the nil-box gate (proves the suffixed cut succeeded)", res.Err)
	}
}
