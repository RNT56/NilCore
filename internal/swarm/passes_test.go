package swarm

import (
	"context"
	"fmt"
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

// TestControllerConvergedMarksRunTerminal asserts a converged run moves its durable run
// row to the terminal status, so a later --resume (which discovers work via
// InFlightSwarm) does NOT re-adopt the finished run.
func TestControllerConvergedMarksRunTerminal(t *testing.T) {
	h := newHarness(t)
	fn := h.fnFromStatusPlan(map[string][]artifact.Status{}) // all green
	c := &Controller{
		Runner:   &Runner{Concurrency: 4, Fn: fn},
		Queue:    h.q,
		Worktree: h.worktree,
		Policy:   PassPolicy{UntilClean: true, MaxPasses: 5},
	}
	shards := shardSet("swarm/run1/0")
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done {
		t.Fatalf("run should converge: %+v", out)
	}
	rows, err := h.q.InFlightSwarm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a converged run must not stay in-flight (got %d rows); --resume would re-adopt it", len(rows))
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
		Runner:        &Runner{Concurrency: 2, Fn: fn},
		Queue:         h.q,
		Worktree:      h.worktree,
		Policy:        PassPolicy{UntilClean: true, MaxPasses: 5},
		Budget:        led,
		GlobalCeiling: 0.0001, // the same wall SetGlobalCeiling got; read non-recordingly
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

// TestControllerBudgetProbeIsNonRecording asserts the MINOR #20 fix: the per-pass global
// headroom check reads the ledger NON-RECORDINGLY (Total() vs GlobalCeiling) and charges
// NOTHING — neither a probe residue against a reserved task nor any global spend. A
// multi-pass run (red→green over 2 passes) is driven and the ledger's Total must stay at
// exactly zero dollars throughout, proving no sub-cent probe was charged per pass.
func TestControllerBudgetProbeIsNonRecording(t *testing.T) {
	h := newHarness(t)
	led := budget.New()
	led.SetGlobalCeiling(100.0) // generous: headroom always exists, so the probe path runs every pass
	plan := map[string][]artifact.Status{
		"swarm/run1/0": {artifact.StatusFail, artifact.StatusPass}, // forces a second pass
	}
	c := &Controller{
		Runner:        &Runner{Concurrency: 2, Fn: h.fnFromStatusPlan(plan)},
		Queue:         h.q,
		Worktree:      h.worktree,
		Policy:        PassPolicy{UntilClean: true, MaxPasses: 5},
		Budget:        led,
		GlobalCeiling: 100.0,
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Passes != 2 {
		t.Fatalf("out = %+v, want Done in 2 passes (so the probe ran on each)", out)
	}
	// The headroom check ran on every pass but charged nothing — Total stays zero. The
	// pre-fix recording probe would have accrued budgetProbe per pass here.
	if tokens, dollars := led.Total(); tokens != 0 || dollars != 0 {
		t.Errorf("ledger Total = (%d tokens, $%.12f), want (0, $0) — the headroom check must not charge", tokens, dollars)
	}
}

// TestControllerLyingGreenDoesNotConverge asserts the I2 keystone for the swarm: a shard
// whose Fn LIES — it returns res.Passed=true while the on-disk artifact is Fail — does
// NOT converge the run. The verifier (the on-disk status, which requeue.Scan reads) is
// authoritative, never the worker self-report. The harness's normal Fn couples the two;
// this test deliberately DECOUPLES them to create the lying-green case the convergence
// test (TestControllerConverges) cannot.
func TestControllerLyingGreenDoesNotConverge(t *testing.T) {
	h := newHarness(t)
	// The lying Fn: write a RED artifact to disk, but self-report Passed=true with a
	// branch (as a compromised/buggy worker might). Scan must still see the red Unit.
	lyingFn := func(ctx context.Context, s Shard) spawn.Result {
		h.mu.Lock()
		h.calls[s.ID]++
		h.mu.Unlock()
		h.writeArtifact(s.ID, artifact.StatusFail)                      // on-disk truth: RED
		return spawn.Result{ID: s.ID, Passed: true, Branch: "task/lie"} // self-report: GREEN (a lie)
	}
	c := &Controller{
		Runner:   &Runner{Concurrency: 1, Fn: lyingFn},
		Queue:    h.q,
		Worktree: h.worktree,
		// Bounded so the permanently-red lie terminates rather than retrying forever; the
		// point is that it never reports Done with a clean tip.
		Policy: PassPolicy{UntilClean: false, MaxPasses: 1},
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 1}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The disk/verifier is authoritative: a red artifact ⇒ a non-empty worklist ⇒ NOT
	// converged, despite the worker's green self-report.
	if out.Done {
		t.Errorf("a lying-green shard (red on disk) must NOT converge the run: out = %+v", out)
	}
	if out.Reason == ReasonConverged {
		t.Errorf("Reason = converged on a red artifact — the self-report was trusted over the verifier (I2 broken)")
	}
	if out.Remaining < 1 {
		t.Errorf("Remaining = %d, want >0 (the red shard is still open)", out.Remaining)
	}
	// The board tally reflects the RED via the ARTIFACT-DERIVED Remaining count
	// (board.Remaining = distinctArtifacts(Scan), NOT the self-report): the red artifact
	// surfaces as a remaining shard, so the scoreboard the operator sees agrees with the
	// disk verdict even though the worker self-reported green.
	if out.Board.Remaining < 1 {
		t.Errorf("Board.Remaining = %d, want >0 — the scoreboard must reflect the on-disk red, not the green lie", out.Board.Remaining)
	}
}

// TestControllerHardCapBacksstopRotatingClaims asserts the MINOR #10 backstop: a worker
// that rotates its claim ids every pass (so the per-Unit retry Ledger never recognizes a
// Unit as exhausted) cannot spin the loop forever. With UntilClean and a generous retry
// budget, the run would loop unbounded on the pre-fix code; the no-progress detector /
// hard cap must stop it RED in a bounded number of passes. The rotating ids keep the
// worklist size constant (one red Unit per pass, always a fresh id), so the no-progress
// detector fires after two such passes.
func TestControllerHardCapBacksstopRotatingClaims(t *testing.T) {
	h := newHarness(t)
	// A worker that writes ONE red claim per pass, with a DIFFERENT claim id each pass —
	// so the retry Ledger (keyed ArtifactID/ClaimID) sees a "fresh" Unit every time and
	// never marks it exhausted. The worklist size stays 1 forever: zero progress.
	rotating := func(ctx context.Context, s Shard) spawn.Result {
		h.mu.Lock()
		n := h.calls[s.ID]
		h.calls[s.ID]++
		h.mu.Unlock()
		a := &artifact.Artifact{
			ID:   s.ID,
			Kind: artifact.KindReport,
			Claims: []artifact.Claim{{
				ID:       fmt.Sprintf("c-rot-%d", n), // ROTATING claim id — defeats the retry Ledger
				Field:    "value",
				Evidence: artifact.Evidence{Value: "x", Status: artifact.StatusFail},
			}},
		}
		blob, _ := artifact.Marshal(a)
		name := strings.ReplaceAll(s.ID, "/", "_") + ".json"
		_ = os.WriteFile(filepath.Join(h.worktree, ".nilcore", "artifacts", name), blob, 0o644)
		return spawn.Result{ID: s.ID, Passed: false}
	}
	c := &Controller{
		Runner:   &Runner{Concurrency: 1, Fn: rotating},
		Queue:    h.q,
		Worktree: h.worktree,
		// UntilClean + a generous per-Unit budget: the ONLY thing that can stop this is the
		// no-progress / hard-cap backstop, not the exhausted rail (each rotated id is fresh).
		Policy:        PassPolicy{UntilClean: true},
		HardMaxPasses: 100, // high enough that the no-progress detector, not the cap, must fire
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 5}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Done {
		t.Errorf("a rotating-claim worker makes no progress and must not converge: %+v", out)
	}
	// The no-progress detector stops it (two passes with a non-shrinking worklist), well
	// before the hard cap — proving the loop is bounded for a non-converging worker.
	if out.Reason != ReasonStalled {
		t.Errorf("Reason = %q, want stalled (no-progress detector)", out.Reason)
	}
	if out.Passes > 4 {
		t.Errorf("ran %d passes before stalling; the no-progress detector must fire promptly", out.Passes)
	}
}

// TestControllerHardCapAbsoluteBackstop asserts the absolute pass cap fires EVEN when a
// worker keeps the worklist nominally shrinking-then-growing so the no-progress detector
// never trips: the hard cap is the last-resort wall. Here a worker alternates the red
// claim COUNT (2 red, then 1 red, then 2 red, …) so Remaining oscillates and never
// stalls two passes running — but the absolute HardMaxPasses still bounds the loop.
func TestControllerHardCapAbsoluteBackstop(t *testing.T) {
	h := newHarness(t)
	oscillating := func(ctx context.Context, s Shard) spawn.Result {
		h.mu.Lock()
		n := h.calls[s.ID]
		h.calls[s.ID]++
		h.mu.Unlock()
		nClaims := 2
		if n%2 == 1 {
			nClaims = 1 // alternate count so Remaining(distinct artifacts) is steady but units oscillate
		}
		claims := make([]artifact.Claim, nClaims)
		for i := range claims {
			claims[i] = artifact.Claim{
				ID:       fmt.Sprintf("c-%d-%d", n, i), // rotating so never exhausted
				Field:    "v",
				Evidence: artifact.Evidence{Value: "x", Status: artifact.StatusFail},
			}
		}
		a := &artifact.Artifact{ID: s.ID, Kind: artifact.KindReport, Claims: claims}
		blob, _ := artifact.Marshal(a)
		name := strings.ReplaceAll(s.ID, "/", "_") + ".json"
		_ = os.WriteFile(filepath.Join(h.worktree, ".nilcore", "artifacts", name), blob, 0o644)
		return spawn.Result{ID: s.ID, Passed: false}
	}
	c := &Controller{
		Runner:        &Runner{Concurrency: 1, Fn: oscillating},
		Queue:         h.q,
		Worktree:      h.worktree,
		Policy:        PassPolicy{UntilClean: true},
		HardMaxPasses: 6, // the absolute wall
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 999}}, shardSet("swarm/run1/0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Done {
		t.Errorf("a non-converging worker must not converge: %+v", out)
	}
	// Whatever rail fires, the loop is BOUNDED by the hard cap: it never exceeds it.
	if out.Passes > 6 {
		t.Errorf("ran %d passes, exceeded the absolute HardMaxPasses=6 backstop", out.Passes)
	}
}

// TestControllerIntegrateBaseRefThreads asserts the load-bearing MAJOR #6 fix: each
// pass folds its green branches onto the PRIOR pass's verified tip, not base HEAD. It
// drives a TWO-green-pass run (shard A green on pass 1; shard B red on pass 1, green on
// pass 2) through a RECORDING base seam that captures the baseRef the Controller hands
// the integrator each pass, then asserts:
//
//   - pass 1 integrates from "" (no prior tip — start at HEAD);
//   - pass 2 integrates from EXACTLY pass 1's returned verified SHA (the tip threaded
//     forward), so pass 2 extends the merged work instead of rebuilding from HEAD;
//   - the final TipBranch is pass 2's verified SHA.
//
// The recording seam is what makes this discriminate: a base-ref-less IntegrateFunc (the
// pre-fix signature) could not even receive the prior tip, so the test would not compile
// against it; here we prove the real value flows pass-to-pass.
func TestControllerIntegrateBaseRefThreads(t *testing.T) {
	h := newHarness(t)
	plan := map[string][]artifact.Status{
		"swarm/run1/0": {artifact.StatusPass},                      // green on pass 1
		"swarm/run1/1": {artifact.StatusFail, artifact.StatusPass}, // red pass 1, green pass 2
	}
	fn := h.fnFromStatusPlan(plan)

	// The recording base seam: capture the baseRef PER PASS and synthesize a verified SHA
	// that embeds the pass number, so pass 2's observed baseRef must equal pass 1's
	// returned SHA for the threading assertion to hold.
	var gotBaseRefs []string
	var gotItems [][]integrate.MergeItem
	pass := 0
	integFn := func(ctx context.Context, baseRef string, order []integrate.MergeItem) (string, []integrate.MergeResult, error) {
		pass++
		gotBaseRefs = append(gotBaseRefs, baseRef)
		gotItems = append(gotItems, order)
		var results []integrate.MergeResult
		for _, it := range order {
			results = append(results, integrate.MergeResult{ID: it.ID, Branch: it.Branch,
				Merged: true, Verified: true, SHA: fmt.Sprintf("tip-p%d", pass)})
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
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shardSet("swarm/run1/0", "swarm/run1/1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Errorf("out = %+v, want Done converged", out)
	}
	if len(gotBaseRefs) < 2 {
		t.Fatalf("integrator called %d times, want >=2 (one per pass)", len(gotBaseRefs))
	}
	// Pass 1 folds from HEAD (no prior tip).
	if gotBaseRefs[0] != "" {
		t.Errorf("pass 1 baseRef = %q, want \"\" (HEAD — no prior tip)", gotBaseRefs[0])
	}
	// Pass 2 folds onto pass 1's verified tip — the THREADING the fix guarantees. If the
	// tip were dead (the bug), pass 2 would re-fold from "" again.
	if gotBaseRefs[1] != "tip-p1" {
		t.Errorf("pass 2 baseRef = %q, want \"tip-p1\" (pass 1's verified tip threaded forward)", gotBaseRefs[1])
	}
	// The final tip is pass 2's verified SHA.
	if out.TipBranch != "tip-p2" {
		t.Errorf("TipBranch = %q, want \"tip-p2\" (final verified tip)", out.TipBranch)
	}
	// MERGED-SET: pass 1 folded shard A; pass 2 folds ONLY the newly-green B — A is
	// already ON the tip pass 2 extends (baseRef tip-p1), so re-merging it would be
	// pure event spam (and, pre-fix, it was re-merged every pass).
	if first := gotItems[0]; len(first) != 1 || first[0].ID != "swarm/run1/0" {
		t.Errorf("pass 1 integrate order = %+v, want exactly shard 0", first)
	}
	last := gotItems[len(gotItems)-1]
	if len(last) != 1 || last[0].ID != "swarm/run1/1" {
		t.Errorf("final integrate order = %+v, want ONLY the newly-green shard 1 (already-merged greens must not re-fold)", last)
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

// posOf returns the index of id in order, or -1. Used by the topo tests to assert
// "dependent merges AFTER dependency" without coupling to the exact interleaving.
func posOf(order []integrate.MergeItem, id string) int {
	for i, it := range order {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// TestMergeOrderTopologicalNotLexical is the direct guard on the B4-swarm.1 fix: with
// >=10 shards whose ids are the DASH form the real sharder emits ("swarm-run-<n>",
// sharder.go) and real Deps edges, mergeOrder must fold each dependent AFTER every
// dependency it was coded on top of. The pre-fix sort.Strings(ids) put "swarm-run-10"
// before "swarm-run-2" (lexical), so a dependent could be folded before its dependency;
// this test fails against that code and passes against the topological emit.
func TestMergeOrderTopologicalNotLexical(t *testing.T) {
	// 11 shards (0..10) so the lexical-vs-numeric divergence is real ("swarm-run-10" <
	// "swarm-run-2" lexically). Chain a dependency so the only valid order is numeric:
	// every shard n>=1 depends on the PRIOR even index, plus shard 10 depends on shard 2.
	const n = 11
	passed := make(map[string]spawn.Result, n)
	deps := make(map[string][]string, n)
	id := func(i int) string { return fmt.Sprintf("swarm-run-%d", i) }
	for i := 0; i < n; i++ {
		passed[id(i)] = spawn.Result{ID: id(i), Passed: true, Branch: "task/" + id(i)}
	}
	// Edges: 1<-0, 2<-1, ... n-1<-n-2 (a strict chain), and additionally 10<-2.
	for i := 1; i < n; i++ {
		deps[id(i)] = []string{id(i - 1)}
	}
	deps[id(10)] = append(deps[id(10)], id(2))

	order := mergeOrder(passed, deps)
	if len(order) != n {
		t.Fatalf("order has %d items, want %d (every green shard with a branch)", len(order), n)
	}

	// The defining assertions: swarm-run-10 must merge AFTER swarm-run-2 AND after its
	// direct chain dependency swarm-run-9 — the exact case lexical sort got wrong.
	if p10, p2 := posOf(order, id(10)), posOf(order, id(2)); p10 < p2 {
		t.Errorf("%s at %d merged before its dependency %s at %d (lexical-sort bug)", id(10), p10, id(2), p2)
	}
	// Every chain edge n -> n-1 must hold (dependent after dependency).
	for i := 1; i < n; i++ {
		if posOf(order, id(i)) < posOf(order, id(i-1)) {
			t.Errorf("%s merged before its dependency %s", id(i), id(i-1))
		}
	}
	// Belt-and-suspenders: every declared edge is respected.
	for child, parents := range deps {
		for _, p := range parents {
			if posOf(order, child) < posOf(order, p) {
				t.Errorf("edge violated: %s merged before dependency %s", child, p)
			}
		}
	}
}

// TestMergeOrderSkipsBranchlessAndDeterministic asserts mergeOrder (a) drops a passed
// shard with no Branch (a verified non-code artifact contributes no MergeItem) and (b)
// is deterministic among independent ready nodes (lexical tie-break), so the fold order
// is stable run-to-run.
func TestMergeOrderSkipsBranchlessAndDeterministic(t *testing.T) {
	passed := map[string]spawn.Result{
		"swarm-run-0": {ID: "swarm-run-0", Passed: true, Branch: "task/a"},
		"swarm-run-1": {ID: "swarm-run-1", Passed: true}, // no Branch: report-only, skipped
		"swarm-run-2": {ID: "swarm-run-2", Passed: true, Branch: "task/c"},
	}
	deps := map[string][]string{} // all independent
	first := mergeOrder(passed, deps)
	if len(first) != 2 {
		t.Fatalf("order has %d items, want 2 (branchless shard dropped)", len(first))
	}
	// Independent ready nodes emit in lexical id order: 0 then 2.
	if first[0].ID != "swarm-run-0" || first[1].ID != "swarm-run-2" {
		t.Errorf("order = %v, want [swarm-run-0 swarm-run-2] (deterministic)", []string{first[0].ID, first[1].ID})
	}
	// Determinism across repeated calls (map iteration is randomized; the sort fixes it).
	for i := 0; i < 20; i++ {
		again := mergeOrder(passed, deps)
		if again[0].ID != first[0].ID || again[1].ID != first[1].ID {
			t.Fatalf("non-deterministic order on iteration %d: %v vs %v", i, again, first)
		}
	}
}

// shardSetWithDeps builds a green-on-pass-1 shard set carrying real Deps, for the
// end-to-end integration-order test. Each shard's Goal is trivial; the deps map is
// id -> dependency ids.
func shardSetWithDeps(deps map[string][]string, ids ...string) []Shard {
	out := make([]Shard, len(ids))
	for i, id := range ids {
		out[i] = Shard{ID: id, Goal: "g", Kind: artifact.KindReport, State: ShardQueued, Deps: deps[id]}
	}
	return out
}

// TestControllerIntegrateRespectsDeps drives a full Run through the Controller and
// asserts the order the IntegrateFunc receives folds dependents after dependencies —
// the B4-swarm.1 fix observed end-to-end (not just on the helper). Ten shards with a
// dash-form id (matching the real sharder) and a chain of Deps prove the integrator
// never sees swarm-run-10 before swarm-run-2.
func TestControllerIntegrateRespectsDeps(t *testing.T) {
	h := newHarness(t)
	const n = 11
	id := func(i int) string { return fmt.Sprintf("swarm-run-%d", i) }
	ids := make([]string, n)
	plan := map[string][]artifact.Status{}
	for i := 0; i < n; i++ {
		ids[i] = id(i)
		plan[id(i)] = []artifact.Status{artifact.StatusPass} // all green pass 1
	}
	deps := map[string][]string{}
	for i := 1; i < n; i++ {
		deps[id(i)] = []string{id(i - 1)}
	}
	deps[id(10)] = append(deps[id(10)], id(2))

	fn := h.fnFromStatusPlan(plan)
	var lastOrder []integrate.MergeItem
	integFn := func(ctx context.Context, baseRef string, order []integrate.MergeItem) (string, []integrate.MergeResult, error) {
		lastOrder = order
		var results []integrate.MergeResult
		for _, it := range order {
			results = append(results, integrate.MergeResult{ID: it.ID, Branch: it.Branch,
				Merged: true, Verified: true, SHA: "tip"})
		}
		return "integrate/x", results, nil
	}

	c := &Controller{
		Runner:    &Runner{Concurrency: 3, Fn: fn},
		Queue:     h.q,
		Worktree:  h.worktree,
		Policy:    PassPolicy{UntilClean: true, MaxPasses: 3},
		Integrate: integFn,
	}
	out, err := c.Run(context.Background(), SwarmState{RunID: "run1", Ledger: requeue.Ledger{MaxAttempts: 3}}, shardSetWithDeps(deps, ids...))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != ReasonConverged {
		t.Fatalf("out = %+v, want Done converged", out)
	}
	if len(lastOrder) != n {
		t.Fatalf("integrate order has %d items, want %d", len(lastOrder), n)
	}
	// The dependent must merge after BOTH its chain dep (9) and its extra dep (2).
	if posOf(lastOrder, id(10)) < posOf(lastOrder, id(2)) {
		t.Errorf("%s merged before dependency %s in the integrator's order", id(10), id(2))
	}
	for i := 1; i < n; i++ {
		if posOf(lastOrder, id(i)) < posOf(lastOrder, id(i-1)) {
			t.Errorf("%s merged before dependency %s in the integrator's order", id(i), id(i-1))
		}
	}
}
