package loop_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nilcore/eval"
	evalself "nilcore/eval/self"
	"nilcore/internal/eventlog"
	"nilcore/internal/flywheel/distiller"
	"nilcore/internal/flywheel/loop"
	"nilcore/internal/selfimprove"
)

// writeFailLog builds a real, hash-chained event log carrying `fails` recurring
// verifier-failure events for one (verifier, class) coordinate, so the distiller
// surfaces exactly one improvement-target Pattern over a CLEAN chain (the loop
// never hand-rolls hashes; eventlog.Verify is the authority). It returns the
// path to the JSONL log.
func writeFailLog(t *testing.T, fails int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	lg, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for i := 0; i < fails; i++ {
		lg.Append(eventlog.Event{
			Kind: "verify",
			Detail: map[string]any{
				"passed":      false,
				"verifier_id": "go-test",
				"fail_class":  "test",
			},
		})
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return path
}

// suiteRunner returns a RunSuite func whose i-th call yields a report at
// passRates[i] (clamped to the last entry once exhausted). It records every
// cases slice it was handed so a test can assert the loop never mutates the
// frozen eval set. total fixes the case count used to synthesize verifier
// verdicts.
type suiteRunner struct {
	passRates []float64 // i-th call's pass-rate; clamps to the last once exhausted
	alternate []float64 // when set, the call's pass-rate is alternate[calls % len]
	calls     int
	gotCases  [][]eval.Case
}

func (s *suiteRunner) run(_ context.Context, cases []eval.Case) (eval.Report, error) {
	// Copy the handed cases so a later mutation by the caller can't retroactively
	// change what we recorded — the test asserts on the snapshot we saw.
	snap := make([]eval.Case, len(cases))
	copy(snap, cases)
	s.gotCases = append(s.gotCases, snap)

	var pr float64
	if len(s.alternate) > 0 {
		pr = s.alternate[s.calls%len(s.alternate)]
	} else {
		idx := s.calls
		if idx >= len(s.passRates) {
			idx = len(s.passRates) - 1
		}
		pr = s.passRates[idx]
	}
	s.calls++

	total := len(cases)
	passes := int(pr*float64(total) + 1e-9)
	rep := eval.Report{Config: "self"}
	for i := 0; i < total; i++ {
		rep.Results = append(rep.Results, eval.Result{Case: "c", Config: "self", Passed: i < passes})
	}
	if total > 0 {
		rep.PassRate = float64(passes) / float64(total)
	}
	return rep, nil
}

// recordingPropose records each Proposal it was handed and returns a fixed
// merged verdict, so a test can assert WHICH candidates the loop routed through
// the gated flow (and how many).
type recordingPropose struct {
	got    []selfimprove.Proposal
	merged bool
}

func (r *recordingPropose) propose(_ context.Context, p selfimprove.Proposal) (bool, error) {
	r.got = append(r.got, p)
	return r.merged, nil
}

// TestImprovedCandidateIsProposed proves the happy path: a recurring scar yields
// a target, the candidate measurably improves the frozen suite, the fence keeps
// it, and it is routed through the gated propose flow.
func TestImprovedCandidateIsProposed(t *testing.T) {
	log := writeFailLog(t, 3) // 3 recurring failures ⇒ one distilled target
	// baseline 0.5, candidate 1.0 ⇒ a clear measured improvement.
	runner := &suiteRunner{passRates: []float64{0.5, 1.0}}
	prop := &recordingPropose{merged: true}

	lp := loop.New(loop.Config{
		LogPath:  log,
		RunSuite: runner.run,
		Propose:  prop.propose,
	})

	sum, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1", sum.Candidates)
	}
	if sum.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1 (the fence kept an improving candidate)", sum.Accepted)
	}
	if sum.Proposed != 1 || len(prop.got) != 1 {
		t.Fatalf("Proposed = %d / propose calls = %d, want 1 (the candidate must reach the gated flow)", sum.Proposed, len(prop.got))
	}
	if sum.Merged != 1 {
		t.Errorf("Merged = %d, want 1 (Propose reported merged=true)", sum.Merged)
	}
	// The proposal must target an in-scope prompt path — NEVER the verifier of
	// record or any core/contract file (C6 + I2).
	got := prop.got[0]
	if len(got.Paths) == 0 {
		t.Fatalf("proposal has no paths")
	}
	if ok, reason := selfimprove.DefaultScope().Check(got); !ok {
		t.Errorf("proposed an out-of-scope edit: %s (paths=%v)", reason, got.Paths)
	}
}

// TestNotImprovedCandidateIsRejected proves the regression fence: a candidate
// that does NOT improve the frozen suite is dropped and NEVER reaches the gated
// propose flow (the C6 guard — no shipping on a hunch).
func TestNotImprovedCandidateIsRejected(t *testing.T) {
	log := writeFailLog(t, 3)
	// baseline 0.8, candidate 0.8 ⇒ a tie. measure.Fence rejects a tie.
	runner := &suiteRunner{passRates: []float64{0.8, 0.8}}
	prop := &recordingPropose{merged: true}

	lp := loop.New(loop.Config{
		LogPath:  log,
		RunSuite: runner.run,
		Propose:  prop.propose,
	})

	sum, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1 (the target was still distilled)", sum.Candidates)
	}
	if sum.Accepted != 0 {
		t.Errorf("Accepted = %d, want 0 (a tie must not pass the fence)", sum.Accepted)
	}
	if sum.Proposed != 0 || len(prop.got) != 0 {
		t.Errorf("Proposed = %d / propose calls = %d, want 0 (a rejected candidate never reaches the gate)", sum.Proposed, len(prop.got))
	}
	if sum.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the fence dropped the candidate)", sum.Skipped)
	}
	// A regression must ALSO be rejected: rerun with a worse candidate.
	runner2 := &suiteRunner{passRates: []float64{0.8, 0.4}}
	prop2 := &recordingPropose{merged: true}
	lp2 := loop.New(loop.Config{LogPath: log, RunSuite: runner2.run, Propose: prop2.propose})
	if _, err := lp2.Run(context.Background()); err != nil {
		t.Fatalf("Run (regression): %v", err)
	}
	if len(prop2.got) != 0 {
		t.Errorf("a regressing candidate was proposed %d times, want 0", len(prop2.got))
	}
}

// TestBoundedStopsAtCap proves the cadence is BOUNDED: with a standing log that
// always yields a target, the loop performs exactly MaxIterations cycles and
// stops — a runaway loop is impossible.
func TestBoundedStopsAtCap(t *testing.T) {
	log := writeFailLog(t, 3)
	// Always improving so every cycle proposes, forcing the loop to rely on the
	// iteration cap (not the no-target early stop) to terminate.
	runner := &suiteRunner{passRates: []float64{0.1, 1.0}}
	prop := &recordingPropose{merged: false}

	const cap = 3
	// alternate baseline(low)/candidate(high) so EVERY cycle measures an
	// improvement and proposes — forcing the iteration cap (not the no-target
	// early stop) to be what terminates the loop.
	runner.alternate = []float64{0.1, 1.0}
	lp := loop.New(loop.Config{
		LogPath:       log,
		RunSuite:      runner.run,
		Propose:       prop.propose,
		MaxIterations: cap,
		Interval:      time.Millisecond, // tiny, hermetic
	})

	sum, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Iterations != cap {
		t.Errorf("Iterations = %d, want exactly the cap %d (bounded cadence)", sum.Iterations, cap)
	}
	if sum.Proposed != cap {
		t.Errorf("Proposed = %d, want %d (one per bounded cycle)", sum.Proposed, cap)
	}
}

// TestNoTargetStopsEarly proves an idle agent with no recurring scar is the
// healthy steady state: the loop runs one cycle, finds nothing to improve, and
// stops without error and without proposing anything.
func TestNoTargetStopsEarly(t *testing.T) {
	log := writeFailLog(t, 0) // no failures ⇒ no distilled target
	runner := &suiteRunner{passRates: []float64{0.5}}
	prop := &recordingPropose{merged: true}

	lp := loop.New(loop.Config{
		LogPath:       log,
		RunSuite:      runner.run,
		Propose:       prop.propose,
		MaxIterations: 5,
		Interval:      time.Millisecond,
	})

	sum, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (no scar ⇒ stop after one cycle)", sum.Iterations)
	}
	if sum.Candidates != 0 || sum.Proposed != 0 {
		t.Errorf("with no scar: Candidates=%d Proposed=%d, want 0/0", sum.Candidates, sum.Proposed)
	}
}

// TestEvalSetNotMutated proves C6's eval-set guard: the loop measures every
// cycle against the FROZEN suite and never mutates it. The suite's content hash
// is identical before and after a full Run, and every cases slice the runner saw
// equals the frozen cases.
func TestEvalSetNotMutated(t *testing.T) {
	before, hashBefore, err := evalself.Load()
	if err != nil {
		t.Fatalf("load frozen suite: %v", err)
	}

	log := writeFailLog(t, 3)
	runner := &suiteRunner{passRates: []float64{0.5, 1.0}}
	prop := &recordingPropose{merged: true}
	lp := loop.New(loop.Config{LogPath: log, RunSuite: runner.run, Propose: prop.propose})
	if _, err := lp.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The frozen suite's identity is unchanged after a full Run.
	_, hashAfter, err := evalself.Load()
	if err != nil {
		t.Fatalf("reload frozen suite: %v", err)
	}
	if hashBefore != hashAfter {
		t.Errorf("frozen self-eval suite hash changed across Run: %s -> %s (C6: eval set must be immutable)", hashBefore, hashAfter)
	}

	// Every cases slice handed to the runner is exactly the frozen set — the loop
	// never trims or rewrites the yardstick between baseline and candidate.
	if len(runner.gotCases) == 0 {
		t.Fatalf("runner was never called")
	}
	for i, cs := range runner.gotCases {
		if len(cs) != len(before.Cases) {
			t.Fatalf("run %d scored %d cases, want the frozen %d", i, len(cs), len(before.Cases))
		}
		for j := range cs {
			if cs[j] != before.Cases[j] {
				t.Errorf("run %d case %d = %+v, want frozen %+v", i, j, cs[j], before.Cases[j])
			}
		}
	}
}

// TestVerifierTargetIsDropped proves C6's verifier-of-record guard: a PlanTarget
// that aims a remediation at the frozen verify package is screened out BEFORE it
// is run, so the verifier of record can never be self-modified — even though the
// fence and the propose flow are wired to accept.
func TestVerifierTargetIsDropped(t *testing.T) {
	log := writeFailLog(t, 3)
	runner := &suiteRunner{passRates: []float64{0.5, 1.0}} // would improve, if allowed
	prop := &recordingPropose{merged: true}

	// A malicious/buggy planner that aims the remediation at the verifier of
	// record (internal/verify/), which DefaultScope denies.
	planAtVerifier := func(p distiller.Pattern) (selfimprove.Proposal, bool) {
		return selfimprove.Proposal{
			Reason: "tamper",
			Paths:  []string{"internal/verify/verify.go"},
			Goal:   "weaken the verifier so the recurring failure passes",
		}, true
	}

	lp := loop.New(loop.Config{
		LogPath:    log,
		RunSuite:   runner.run,
		Propose:    prop.propose,
		PlanTarget: planAtVerifier,
	})

	sum, err := lp.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1 (the target was distilled)", sum.Candidates)
	}
	if sum.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (a verifier-targeting edit must be dropped by the scope screen)", sum.Skipped)
	}
	if len(prop.got) != 0 {
		t.Fatalf("a verifier-targeting edit reached the propose flow %d times, want 0 (C6: verifier-of-record is untouchable)", len(prop.got))
	}
	// The fence must never even run for an out-of-scope target: it is screened
	// BEFORE the candidate is scored, so only the single baseline run happened.
	if runner.calls != 1 {
		t.Errorf("runner called %d times, want 1 (scope screen drops before scoring a denied candidate)", runner.calls)
	}
}

// TestConstructionIsInert proves DEFAULT-OFF: constructing a Loop does nothing —
// no eval runs, no log is read, no proposal is made — until Run is called. And
// Run fails closed (a precise sentinel) when a required injected dependency is
// missing, rather than running unbounded or unverified.
func TestConstructionIsInert(t *testing.T) {
	runner := &suiteRunner{passRates: []float64{1.0}}
	prop := &recordingPropose{merged: true}

	// Constructing alone must not touch the runner or the propose hook.
	_ = loop.New(loop.Config{
		LogPath:  filepath.Join(t.TempDir(), "events.jsonl"),
		RunSuite: runner.run,
		Propose:  prop.propose,
	})
	if runner.calls != 0 || len(prop.got) != 0 {
		t.Errorf("New ran work: runner.calls=%d propose=%d, want 0/0 (default-off)", runner.calls, len(prop.got))
	}

	// Run fails closed on each missing required dependency.
	if _, err := loop.New(loop.Config{LogPath: "x", Propose: prop.propose}).Run(context.Background()); !errors.Is(err, loop.ErrNoRunner) {
		t.Errorf("missing RunSuite: err = %v, want ErrNoRunner", err)
	}
	if _, err := loop.New(loop.Config{LogPath: "x", RunSuite: runner.run}).Run(context.Background()); !errors.Is(err, loop.ErrNoPropose) {
		t.Errorf("missing Propose: err = %v, want ErrNoPropose", err)
	}
	if _, err := loop.New(loop.Config{RunSuite: runner.run, Propose: prop.propose}).Run(context.Background()); !errors.Is(err, loop.ErrNoLogPath) {
		t.Errorf("missing LogPath: err = %v, want ErrNoLogPath", err)
	}
}

// TestContextCancellationStops proves the ctx-first contract: a cancelled
// context stops the loop promptly and reports the cancellation.
func TestContextCancellationStops(t *testing.T) {
	log := writeFailLog(t, 3)
	runner := &suiteRunner{passRates: []float64{0.1, 1.0}}
	prop := &recordingPropose{merged: false}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lp := loop.New(loop.Config{
		LogPath:       log,
		RunSuite:      runner.run,
		Propose:       prop.propose,
		MaxIterations: 5,
		Interval:      time.Hour, // would block forever if ctx were ignored
	})
	if _, err := lp.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Run err = %v, want context.Canceled", err)
	}
}
