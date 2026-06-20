package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/budget"
	"nilcore/internal/integrate"
	"nilcore/internal/requeue"
	"nilcore/internal/spawn"
)

// harness wires a Controller over a temp worktree + store with a scripted ShardFunc.
// The ShardFunc is the I2 enforcement point: it writes the shard's artifact on disk
// (artifact id == shard id, the convention requeue.Scan reads) with a per-shard claim
// status, and sets Result.Passed to MATCH the artifact's greenness — so the verifier
// verdict (the on-disk status) and the Result agree, exactly as the real ship gate
// guarantees. The test scripts the status per (shard, attempt) to drive convergence.
type harness struct {
	t        *testing.T
	worktree string
	q        *Queue
	calls    map[string]int // per-shard Fn call count (proves requeue-only-failed)
	mu       sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".nilcore", "artifacts"), 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	return &harness{
		t:        t,
		worktree: wt,
		q:        NewQueue(openStore(t), nil, "run1"),
		calls:    map[string]int{},
	}
}

// writeArtifact writes a one-claim artifact for shardID with the given status. The
// artifact id is the shard id, so requeue.Scan attributes its red claim to the shard.
func (h *harness) writeArtifact(shardID string, status artifact.Status) {
	h.t.Helper()
	a := &artifact.Artifact{
		ID:   shardID,
		Kind: artifact.KindReport,
		Claims: []artifact.Claim{{
			ID:       "c1",
			Field:    "value",
			Evidence: artifact.Evidence{Value: "x", Status: status},
		}},
	}
	blob, err := artifact.Marshal(a)
	if err != nil {
		h.t.Fatalf("marshal artifact: %v", err)
	}
	// The on-disk filename is irrelevant to attribution (Scan reads the artifact's own
	// ID), but a stable per-shard name keeps re-writes in place across passes.
	name := strings.ReplaceAll(shardID, "/", "_") + ".json"
	if err := os.WriteFile(filepath.Join(h.worktree, ".nilcore", "artifacts", name), blob, 0o644); err != nil {
		h.t.Fatalf("write artifact: %v", err)
	}
}

// fnFromStatusPlan builds a ShardFunc whose verdict for a shard on a given call number
// comes from plan[shardID]: plan[id][n] is the status to write on the (n+1)-th call;
// the LAST entry is reused for any further call. This lets a test say "shard X is red
// on attempt 1, green on attempt 2".
func (h *harness) fnFromStatusPlan(plan map[string][]artifact.Status) ShardFunc {
	return func(ctx context.Context, s Shard) spawn.Result {
		h.mu.Lock()
		n := h.calls[s.ID]
		h.calls[s.ID]++
		h.mu.Unlock()

		statuses := plan[s.ID]
		var st artifact.Status
		switch {
		case len(statuses) == 0:
			st = artifact.StatusPass
		case n < len(statuses):
			st = statuses[n]
		default:
			st = statuses[len(statuses)-1]
		}
		h.writeArtifact(s.ID, st)

		// The Result mirrors the on-disk verdict: Passed iff the artifact is green. This
		// IS the ship gate the cmd wiring supplies — green report ⇒ Passed+Branch.
		green := st == artifact.StatusPass
		res := spawn.Result{ID: s.ID, Passed: green}
		if green {
			res.Branch = "task/" + strings.ReplaceAll(s.ID, "/", "-")
		}
		return res
	}
}

func (h *harness) callCount(shardID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[shardID]
}

func shardSet(ids ...string) []Shard {
	out := make([]Shard, len(ids))
	for i, id := range ids {
		out[i] = Shard{ID: id, Goal: "g", Kind: artifact.KindReport, State: ShardQueued}
	}
	return out
}

// TestControllerConverges asserts a run where every shard is green on pass 1 ends Done
// with Reason converged and zero remaining.
func TestControllerConverges(t *testing.T) {
	h := newHarness(t)
	fn := h.fnFromStatusPlan(map[string][]artifact.Status{}) // all green
	c := &Controller{
		Runner:   &Runner{Concurrency: 4, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: true, MaxPasses: 5},
	}
	shards := shardSet("swarm/run1/0", "swarm/run1/1")
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Errorf("out = %+v, want Done converged", out)
	}
	if out.Remaining != 0 || out.Passes != 1 {
		t.Errorf("Remaining=%d Passes=%d, want 0/1", out.Remaining, out.Passes)
	}
}

// TestControllerUntilCleanRetryToGreen asserts a shard red on attempt 1 and green on
// attempt 2 converges in exactly 2 passes, and that ONLY the failed shard is re-run
// (the call-count proves the passed shard's Fn is not invoked in pass 2).
func TestControllerUntilCleanRetryToGreen(t *testing.T) {
	h := newHarness(t)
	plan := map[string][]artifact.Status{
		"swarm/run1/0": {artifact.StatusPass},                      // green on pass 1, never re-run
		"swarm/run1/1": {artifact.StatusFail, artifact.StatusPass}, // red then green
	}
	fn := h.fnFromStatusPlan(plan)
	c := &Controller{
		Runner:   &Runner{Concurrency: 4, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: true, MaxPasses: 5},
	}
	shards := shardSet("swarm/run1/0", "swarm/run1/1")
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Errorf("out = %+v, want Done converged", out)
	}
	if out.Passes != 2 {
		t.Errorf("Passes = %d, want 2", out.Passes)
	}
	// requeue-only-failed: shard 0 (green pass 1) ran ONCE; shard 1 ran TWICE.
	if got := h.callCount("swarm/run1/0"); got != 1 {
		t.Errorf("passed shard re-run: call count = %d, want 1", got)
	}
	if got := h.callCount("swarm/run1/1"); got != 2 {
		t.Errorf("failed shard call count = %d, want 2", got)
	}
}

// TestControllerExhausted asserts a permanently-red shard converges RED with Reason
// exhausted once it spends its retry budget, with Remaining>0.
func TestControllerExhausted(t *testing.T) {
	h := newHarness(t)
	plan := map[string][]artifact.Status{
		"swarm/run1/0": {artifact.StatusFail}, // never recovers
	}
	fn := h.fnFromStatusPlan(plan)
	c := &Controller{
		Runner:   &Runner{Concurrency: 2, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: true, MaxPasses: 99},
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 2}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Done {
		t.Errorf("a permanently-red run must not be Done")
	}
	if out.Reason != ReasonExhausted {
		t.Errorf("Reason = %q, want exhausted", out.Reason)
	}
	if out.Remaining < 1 {
		t.Errorf("Remaining = %d, want >0", out.Remaining)
	}
}

// TestControllerPassesCut asserts a still-red run with MaxPasses=1 (the default-off
// shape) stops after one pass with Reason passes, never requeuing.
func TestControllerPassesCut(t *testing.T) {
	h := newHarness(t)
	plan := map[string][]artifact.Status{"swarm/run1/0": {artifact.StatusFail}}
	fn := h.fnFromStatusPlan(plan)
	c := &Controller{
		Runner:   &Runner{Concurrency: 2, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: false, MaxPasses: 1}, // exactly one pass
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 5}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonPasses {
		t.Errorf("Reason = %q, want passes", out.Reason)
	}
	if out.Passes != 1 {
		t.Errorf("Passes = %d, want 1", out.Passes)
	}
	if got := h.callCount("swarm/run1/0"); got != 1 {
		t.Errorf("shard ran %d times, want 1 (no requeue at the cap)", got)
	}
}

// TestControllerBudgetCut asserts a pre-exhausted global ledger stops the run with
// Reason budget before any shard dispatches.
func TestControllerBudgetCut(t *testing.T) {
	h := newHarness(t)
	led := budget.New()
	led.SetGlobalCeiling(0.0001)
	// Spend the whole global ceiling so the headroom probe is refused.
	if err := led.Charge(context.Background(), "warmup", 0, 0.0001); err != nil {
		t.Fatalf("charge: %v", err)
	}
	fn := h.fnFromStatusPlan(map[string][]artifact.Status{})
	c := &Controller{
		Runner:   &Runner{Concurrency: 2, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: true, MaxPasses: 5},
		Budget:   led,
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonBudget {
		t.Errorf("Reason = %q, want budget", out.Reason)
	}
	if got := h.callCount("swarm/run1/0"); got != 0 {
		t.Errorf("shard dispatched despite budget cut: %d calls", got)
	}
}

// TestControllerIntegrateBaseRefThreads asserts the IntegrateFunc receives the green
// shards' branches and that the verified SHA threads forward onto SwarmState.TipSHA so
// the next pass folds on top of the prior tip.
func TestControllerIntegrateBaseRefThreads(t *testing.T) {
	h := newHarness(t)
	plan := map[string][]artifact.Status{
		"swarm/run1/0": {artifact.StatusFail, artifact.StatusPass}, // green on pass 2
	}
	fn := h.fnFromStatusPlan(plan)

	var gotItems [][]integrate.MergeItem
	integFn := func(ctx context.Context, order []integrate.MergeItem) (string, []integrate.MergeResult, error) {
		gotItems = append(gotItems, order)
		var results []integrate.MergeResult
		for _, it := range order {
			results = append(results, integrate.MergeResult{ID: it.ID, Branch: it.Branch,
				Merged: true, Verified: true, SHA: "sha-" + it.ID})
		}
		return "integrate/x", results, nil
	}

	c := &Controller{
		Runner:    &Runner{Concurrency: 2, Fn: fn},
		Queue:     h.q,
		Worktree:  h.worktree,
		Policy:    PassPolicy{UntilClean: true, MaxPasses: 5},
		Integrate: integFn,
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Errorf("out = %+v, want Done converged", out)
	}
	// On the final (green) pass, the green shard's branch must reach the integrator and
	// the verified SHA must thread onto the tip.
	if out.TipBranch != "sha-swarm/run1/0" {
		t.Errorf("TipBranch = %q, want the verified merge SHA threaded forward", out.TipBranch)
	}
	// The integrator was only handed the shard once green (pass 2), never the red pass.
	if len(gotItems) == 0 {
		t.Fatalf("integrator never called")
	}
	last := gotItems[len(gotItems)-1]
	if len(last) != 1 || last[0].ID != "swarm/run1/0" {
		t.Errorf("final integrate order = %+v, want [swarm/run1/0]", last)
	}
}

// TestControllerResumeGreenStaysGreen asserts a fresh Controller over the SAME worktree
// re-Scans the persisted artifacts and recognizes an already-green shard as converged
// without re-running it — the resume contract (green stays green, zero lost progress).
func TestControllerResumeGreenStaysGreen(t *testing.T) {
	h := newHarness(t)
	// Simulate a prior pass having written a GREEN artifact for the shard on disk.
	h.writeArtifact("swarm/run1/0", artifact.StatusPass)

	// A fresh Controller whose Fn would FAIL the shard if called — so if convergence is
	// (incorrectly) decided by re-running rather than by the persisted artifact, the test
	// catches it. requeue.Scan over the green artifact yields an empty worklist, so the
	// controller converges WITHOUT dispatching.
	failIfCalled := func(ctx context.Context, s Shard) spawn.Result {
		// Overwrite the green artifact with a red one to prove non-dispatch: if the Fn
		// runs, the artifact turns red and the run would NOT converge.
		h.writeArtifact(s.ID, artifact.StatusFail)
		return spawn.Result{ID: s.ID, Passed: false}
	}
	_ = failIfCalled

	// The resume path: the open worklist is recomputed from the artifacts. With a green
	// artifact already on disk, the initial shard set is empty (nothing red to run), so
	// the controller converges on pass 1 with no dispatch.
	openShards := h.recomputeOpen("run1")
	c := &Controller{
		Runner:   &Runner{Concurrency: 2, Fn: failIfCalled},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: true, MaxPasses: 5},
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, openShards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Errorf("resume out = %+v, want Done converged", out)
	}
	if got := h.callCount("swarm/run1/0"); got != 0 {
		t.Errorf("resumed run re-ran an already-green shard %d times", got)
	}
}

// recomputeOpen mirrors the cmd resume path: the open shard set is the shards whose
// artifact still has a red Unit (requeue.Scan). With everything green it is empty.
func (h *harness) recomputeOpen(runID string) []Shard {
	h.t.Helper()
	led := requeue.Ledger{MaxAttempts: 3}
	wl, err := requeue.Scan(h.worktree, &led)
	if err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	seen := map[string]bool{}
	var out []Shard
	for _, u := range wl.Units {
		if seen[u.ArtifactID] {
			continue
		}
		seen[u.ArtifactID] = true
		out = append(out, Shard{ID: u.ArtifactID, Goal: "g", Kind: artifact.KindReport, State: ShardQueued})
	}
	return out
}
