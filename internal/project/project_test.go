package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/advisor"
	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// --- hermetic fakes (no network, no real git/sandbox) ------------------------

// fixedVerifier returns a constant verdict; toggleVerifier flips to pass after a
// set number of calls (to drive convergence deterministically).
type fixedVerifier struct {
	pass bool
	err  error
	n    int32
}

func (v *fixedVerifier) Check(context.Context) (verify.Report, error) {
	atomic.AddInt32(&v.n, 1)
	if v.err != nil {
		return verify.Report{}, v.err
	}
	return verify.Report{Passed: v.pass, Output: "out"}, nil
}

// scriptBox is a hermetic sandbox.Sandbox: it returns a scripted Result per command
// (keyed by exact command string), defaulting to exit 0 (runnable). It records every
// command it was asked to run so a test can assert dry-runs happened.
type scriptBox struct {
	results map[string]sandbox.Result
	def     sandbox.Result
	execErr error
	mu      atomic.Int32
	ran     []string
}

func (b *scriptBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	b.mu.Add(1)
	b.ran = append(b.ran, cmd)
	if b.execErr != nil {
		return sandbox.Result{}, b.execErr
	}
	if r, ok := b.results[cmd]; ok {
		return r, nil
	}
	return b.def, nil
}
func (b *scriptBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *scriptBox) Workdir() string { return "/work" }

// replyModel returns a fixed advisor reply (the proposed criteria block).
type replyModel struct{ reply string }

func (replyModel) Model() string { return "advisor-fake" }
func (m replyModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: m.reply}}}, nil
}

// askChannel records the question and returns a scripted yes/no (and optional err).
type askChannel struct {
	yes   bool
	err   error
	asked int32
	lastQ string
}

func (c *askChannel) Ask(_ context.Context, _ string, q string) (bool, error) {
	atomic.AddInt32(&c.asked, 1)
	c.lastQ = q
	return c.yes, c.err
}

// tmpLog opens a fresh append-only log in a temp dir (hermetic; no shared state).
func tmpLog(t *testing.T) *eventlog.Log {
	t.Helper()
	lg, err := eventlog.Open(filepath.Join(t.TempDir(), "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

// crit builds a Criterion gated by a fixed verifier (no sandbox needed).
func crit(cmd string, pass bool) Criterion {
	return Criterion{Command: cmd, Description: "d", Verifier: &fixedVerifier{pass: pass}}
}

// --- JudgeProject: exit-code AND, never an LLM verdict -----------------------

func TestJudgeProject_ExitCodeAnd(t *testing.T) {
	tests := []struct {
		name      string
		proj      verify.Verifier
		criteria  []Criterion
		wantDone  bool
		wantUnmet int
	}{
		{
			name:      "verifier green, no criteria → done",
			proj:      &fixedVerifier{pass: true},
			wantDone:  true,
			wantUnmet: 0,
		},
		{
			name:      "verifier red alone → not done",
			proj:      &fixedVerifier{pass: false},
			wantDone:  false,
			wantUnmet: 1,
		},
		{
			name:      "verifier green but one criterion red → not done",
			proj:      &fixedVerifier{pass: true},
			criteria:  []Criterion{crit("c1", true), crit("c2", false)},
			wantDone:  false,
			wantUnmet: 1,
		},
		{
			name:      "verifier green and all criteria green → done",
			proj:      &fixedVerifier{pass: true},
			criteria:  []Criterion{crit("c1", true), crit("c2", true)},
			wantDone:  true,
			wantUnmet: 0,
		},
		{
			name:      "verifier transport error counts as unmet (a check we could not run is not a pass)",
			proj:      &fixedVerifier{err: errors.New("boom")},
			wantDone:  false,
			wantUnmet: 1,
		},
		{
			name:      "criterion with nil verifier is skipped (covered by VerifyCmd)",
			proj:      &fixedVerifier{pass: true},
			criteria:  []Criterion{{Command: "", Description: "covered"}},
			wantDone:  true,
			wantUnmet: 0,
		},
		{
			name:      "all reds accumulate the unmet count",
			proj:      &fixedVerifier{pass: false},
			criteria:  []Criterion{crit("c1", false), crit("c2", false)},
			wantDone:  false,
			wantUnmet: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, unmet := JudgeProject(context.Background(), tc.proj, tc.criteria)
			if done != tc.wantDone || unmet != tc.wantUnmet {
				t.Errorf("JudgeProject = (%t, %d), want (%t, %d)", done, unmet, tc.wantDone, tc.wantUnmet)
			}
		})
	}
}

// An LLM-flavored "looks done" prose can never flip the verdict: JudgeProject reads
// only exit codes. We assert a red verifier stays not-done regardless of any
// Description text (the only place LLM prose lives).
func TestJudgeProject_LLMProseNeverGates(t *testing.T) {
	c := Criterion{Description: "DONE: everything works, ship it!", Command: "c", Verifier: &fixedVerifier{pass: false}}
	done, unmet := JudgeProject(context.Background(), &fixedVerifier{pass: true}, []Criterion{c})
	if done || unmet != 1 {
		t.Fatalf("LLM prose must not gate done: got done=%t unmet=%d", done, unmet)
	}
}

// --- DeriveAcceptance: advisor proposes, sandbox dry-runs, add-only ----------

func TestDeriveAcceptance_DropsUnrunnable(t *testing.T) {
	// The advisor proposes three criteria; the sandbox says one is unrunnable (a
	// transport error), one is "command not found", and one runs (red is fine).
	adv := advisor.New(replyModel{reply: "health endpoint :: go test -run Health\n" +
		"missing tool :: frobnicate --all\n" +
		"orders persist :: go test -run Orders"}, 0)

	box := &scriptBox{
		results: map[string]sandbox.Result{
			"go test -run Health": {ExitCode: 1},                                // runnable, currently RED — keep
			"frobnicate --all":    {ExitCode: 127, Stderr: "command not found"}, // unrunnable — drop
			"go test -run Orders": {ExitCode: 0},                                // runnable — keep
		},
	}
	lg := tmpLog(t)

	got := DeriveAcceptance(context.Background(), adv, box, "build a service", nil, lg)
	if len(got) != 2 {
		t.Fatalf("kept %d criteria, want 2 (the unrunnable one dropped)", len(got))
	}
	cmds := map[string]bool{}
	for _, c := range got {
		cmds[c.Command] = true
		if c.Verifier == nil {
			t.Errorf("kept criterion %q has no verifier", c.Command)
		}
	}
	if !cmds["go test -run Health"] || !cmds["go test -run Orders"] {
		t.Errorf("kept the wrong criteria: %v", cmds)
	}
	if cmds["frobnicate --all"] {
		t.Error("the unrunnable criterion was not dropped")
	}
}

// Refinement is ADD-ONLY and idempotent: re-deriving with an existing bar never
// removes an existing criterion, and a re-proposed identical command is not
// duplicated.
func TestDeriveAcceptance_AddOnlyIdempotent(t *testing.T) {
	existing := []Criterion{crit("go test -run Health", true)}
	adv := advisor.New(replyModel{reply: "health again :: go test -run Health\n" +
		"new check :: go vet ./..."}, 0)
	box := &scriptBox{def: sandbox.Result{ExitCode: 0}}
	lg := tmpLog(t)

	got := DeriveAcceptance(context.Background(), adv, box, "goal", existing, lg)
	if len(got) != 2 {
		t.Fatalf("got %d criteria, want 2 (existing kept + one new, no dup)", len(got))
	}
	// The original criterion (and its verifier) must survive untouched (add-only).
	if got[0].Command != "go test -run Health" {
		t.Errorf("existing criterion was reordered/removed: %q", got[0].Command)
	}
}

// A nil advisor or a proposal that yields nothing must NOT lower the bar: the
// existing criteria are returned unchanged.
func TestDeriveAcceptance_NeverLowersBar(t *testing.T) {
	existing := []Criterion{crit("go test ./...", true)}
	box := &scriptBox{def: sandbox.Result{ExitCode: 0}}
	lg := tmpLog(t)

	if got := DeriveAcceptance(context.Background(), nil, box, "g", existing, lg); len(got) != 1 {
		t.Errorf("nil advisor changed the bar: %d criteria", len(got))
	}
	emptyAdv := advisor.New(replyModel{reply: "\n\n```\n```\n"}, 0)
	if got := DeriveAcceptance(context.Background(), emptyAdv, box, "g", existing, lg); len(got) != 1 {
		t.Errorf("empty proposal changed the bar: %d criteria", len(got))
	}
}

// --- Loop termination: one DISTINCT Reason per ceiling -----------------------

// baseLoop builds a loop with all required seams wired to deterministic fakes.
func baseLoop(t *testing.T) *Loop {
	t.Helper()
	dir := t.TempDir()
	// a go.mod-less dir → verify.Detect returns "true"; harmless for these tests.
	_ = os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644)
	return &Loop{
		Goal: "build a thing",
		Repo: dir,
		Log:  tmpLog(t),
		Plan: func(_ context.Context, _ string, _ State) (Slice, error) {
			return Slice{Goal: "do work"}, nil
		},
		RunSlice: func(_ context.Context, _ Slice, _ State) (SliceResult, error) {
			return SliceResult{Branch: "task/super.t1", Verified: true}, nil
		},
		Verifier:      func(string) verify.Verifier { return &fixedVerifier{pass: false} },
		MaxIterations: 5,
		MaxNoProgress: 99, // disable the no-progress rail unless a test wants it
	}
}

func TestRun_Converged(t *testing.T) {
	l := baseLoop(t)
	// The project starts RED and the first slice flips it green (the realistic
	// convergence: a slice lands, the tip advances, the next judge pass is done).
	var green atomic.Bool
	l.Verifier = func(string) verify.Verifier { return &togglePass{&green} }
	l.SeedCriteria([]Criterion{{Command: "c1", Verifier: &togglePass{&green}}})
	l.RunSlice = func(_ context.Context, _ Slice, _ State) (SliceResult, error) {
		green.Store(true)
		return SliceResult{Branch: "task/super.t1", Verified: true}, nil
	}

	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonConverged || !out.Done {
		t.Fatalf("got reason=%q done=%t, want converged/true", out.Reason, out.Done)
	}
	if out.Unmet != 0 {
		t.Errorf("converged with unmet=%d, want 0", out.Unmet)
	}
	if out.Branch != "task/super.t1" {
		t.Errorf("converged branch = %q, want the verified tip", out.Branch)
	}
}

func TestRun_MaxIterations(t *testing.T) {
	l := baseLoop(t)
	// Verifier never passes and a slice "succeeds" but never makes the project green
	// → the loop exhausts MaxIterations. No-progress is disabled so the iteration
	// rail (not the stall rail) is the one that fires — proving they are distinct.
	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonMaxIterations {
		t.Fatalf("reason = %q, want max_iterations", out.Reason)
	}
	if out.Iterations != 5 {
		t.Errorf("iterations = %d, want 5", out.Iterations)
	}
	if out.Done {
		t.Error("an unconverged run reported Done")
	}
}

func TestRun_NoProgress(t *testing.T) {
	l := baseLoop(t)
	l.SeedCriteria([]Criterion{crit("c1", false)}) // always red → unmet never drops
	l.MaxNoProgress = 2                            // stall ceiling fires before MaxIterations
	// No channel wired → stop-and-ask defaults to STOP (no ambient authority).

	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonNoProgress {
		t.Fatalf("reason = %q, want no_progress", out.Reason)
	}
	if out.Iterations >= 5 {
		t.Errorf("no-progress fired at iteration %d, should be before MaxIterations(5)", out.Iterations)
	}
}

// With a channel that says "keep going", the no-progress rail resets and the loop
// continues until the iteration ceiling instead — proving the stop-ask bridge.
func TestRun_NoProgress_HumanKeepsGoing(t *testing.T) {
	l := baseLoop(t)
	l.SeedCriteria([]Criterion{crit("c1", false)})
	l.MaxNoProgress = 2
	ch := &askChannel{yes: true}
	l.Channel = ch

	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonMaxIterations {
		t.Fatalf("reason = %q, want max_iterations (human kept it going)", out.Reason)
	}
	if atomic.LoadInt32(&ch.asked) == 0 {
		t.Error("the human was never asked")
	}
}

func TestRun_Budget(t *testing.T) {
	l := baseLoop(t)
	led := budget.New()
	led.SetGlobalCeiling(1.0)
	// Pre-spend to the ceiling so the loop's zero-charge probe trips immediately.
	if err := led.Charge(context.Background(), "x", 1, 1.0); err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	l.Budget = led

	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonBudget {
		t.Fatalf("reason = %q, want budget", out.Reason)
	}
}

func TestRun_Deadline(t *testing.T) {
	l := baseLoop(t)
	l.Deadline = time.Now().Add(-time.Second) // already past → wall-clock rail fires
	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonDeadline {
		t.Fatalf("reason = %q, want deadline", out.Reason)
	}
}

func TestRun_CtxCancelled(t *testing.T) {
	l := baseLoop(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := l.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonDeadline { // ctx maps to the single wall-clock rail
		t.Fatalf("reason = %q, want deadline (ctx)", out.Reason)
	}
}

// A broken audit trail halts the run with a distinct reason and a surfaced error
// (I5: an unverifiable history is worse than continuing).
func TestRun_LogBroken(t *testing.T) {
	l := baseLoop(t)
	// Close the log file so the next Append fails → Log.Err() is non-nil.
	_ = l.Log.Close()
	l.Log.Append(eventlog.Event{Task: "x", Kind: "trip"}) // forces a write error
	out, err := l.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error on a broken audit trail")
	}
	if out.Reason != ReasonLogBroken {
		t.Fatalf("reason = %q, want log_broken", out.Reason)
	}
}

// --- Recovery ladder: never abort; converge if a later slice succeeds --------

// A Plan that errors the first two passes then succeeds must NOT abort: the ladder
// narrows/switches and the loop keeps going, converging once work lands.
func TestRun_PlanErrorsRecover(t *testing.T) {
	l := baseLoop(t)
	var planCalls int32
	l.Plan = func(_ context.Context, _ string, _ State) (Slice, error) {
		if atomic.AddInt32(&planCalls, 1) <= 2 {
			return Slice{}, errors.New("planner hiccup")
		}
		return Slice{Goal: "ok"}, nil
	}
	// Once a real slice runs, the verifier passes → converge.
	var verifierPass atomic.Bool
	l.Verifier = func(string) verify.Verifier { return &togglePass{&verifierPass} }
	l.RunSlice = func(_ context.Context, _ Slice, _ State) (SliceResult, error) {
		verifierPass.Store(true)
		return SliceResult{Branch: "task/super.t1", Verified: true}, nil
	}
	l.SeedCriteria([]Criterion{{Command: "c", Verifier: &togglePass{&verifierPass}}})

	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != ReasonConverged {
		t.Fatalf("reason = %q, want converged after recovery", out.Reason)
	}
	if atomic.LoadInt32(&planCalls) < 3 {
		t.Errorf("ladder did not retry planning: %d calls", planCalls)
	}
}

// A RunSlice that always errors must climb the ladder to stop (it never aborts with
// a Go error) and terminate on a no-progress reason, not a panic.
func TestRun_SliceErrorsClimbToStop(t *testing.T) {
	l := baseLoop(t)
	l.Advisor = advisor.New(replyModel{reply: "try a different approach"}, 0)
	l.RunSlice = func(_ context.Context, _ Slice, _ State) (SliceResult, error) {
		return SliceResult{}, errors.New("slice exploded")
	}
	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run must not return an error on repeated slice failure: %v", err)
	}
	if out.Reason != ReasonNoProgress && out.Reason != ReasonMaxIterations {
		t.Fatalf("reason = %q, want a graceful stop", out.Reason)
	}
}

// togglePass is a verifier whose pass state follows an external atomic flag, so a
// test can flip the project from red to green when a slice lands.
type togglePass struct{ on *atomic.Bool }

func (v *togglePass) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: v.on.Load()}, nil
}

// --- converge + gated promote ------------------------------------------------

// On convergence with a Gate seam wired, the loop attempts exactly one structured
// PromoteToBase and records the approver's decision — the single irreversible step.
func TestRun_ConvergePromotes(t *testing.T) {
	l := baseLoop(t)
	var green atomic.Bool
	l.Verifier = func(string) verify.Verifier { return &togglePass{&green} }
	l.SeedCriteria([]Criterion{{Command: "c1", Verifier: &togglePass{&green}}})
	l.RunSlice = func(_ context.Context, _ Slice, _ State) (SliceResult, error) {
		green.Store(true)
		return SliceResult{Branch: "task/super.t1", Verified: true}, nil
	}

	var gated int32
	var gotAction policy.GateAction
	l.Gate = func(a policy.GateAction) bool {
		atomic.AddInt32(&gated, 1)
		gotAction = a
		return true
	}
	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || !out.Promoted {
		t.Fatalf("converged run not promoted: done=%t promoted=%t", out.Done, out.Promoted)
	}
	if atomic.LoadInt32(&gated) != 1 {
		t.Errorf("gate called %d times, want exactly 1", gated)
	}
	if gotAction.Type != policy.PromoteToBase {
		t.Errorf("gate action type = %v, want PromoteToBase", gotAction.Type)
	}
}

// A denied promote still reports Done (the verifier passed) but Promoted=false —
// the gate is the human's call, separate from done-ness.
func TestRun_PromoteDenied(t *testing.T) {
	l := baseLoop(t)
	var green atomic.Bool
	l.Verifier = func(string) verify.Verifier { return &togglePass{&green} }
	l.SeedCriteria([]Criterion{{Command: "c1", Verifier: &togglePass{&green}}})
	l.RunSlice = func(_ context.Context, _ Slice, _ State) (SliceResult, error) {
		green.Store(true)
		return SliceResult{Branch: "task/super.t1", Verified: true}, nil
	}
	l.Gate = func(policy.GateAction) bool { return false }

	out, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Promoted {
		t.Fatalf("denied promote: done=%t promoted=%t, want true/false", out.Done, out.Promoted)
	}
}
