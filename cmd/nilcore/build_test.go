package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/project"
)

// build_test.go is the hermetic test of the `nilcore build` wiring (P5-T02). It
// drives NO container and NO network: it injects a scripted model.Provider, a temp
// git repo, and a shared budget.Ledger, then asserts the three load-bearing
// properties of the wiring:
//
//   - the flags parse with the §9 defaults (so the operator surface is correct);
//   - the metered providers buildStack constructs share ONE ledger that becomes a
//     real wall — a charge past the ceiling returns budget.ErrCeiling (closing the
//     dead-budget blocker, design §7);
//   - the project loop buildStack assembles aborts a runaway through that ceiling,
//     terminating with project.ReasonBudget rather than spinning.
//
// The full run (real spawn/integrate over a container) is deliberately NOT
// exercised here — it needs a sandbox — so the wiring is validated with the budget
// rail tripping before any container work is reached.

// fakeProvider is a scripted model.Provider for hermetic wiring tests. Every
// Complete returns a fixed finish-shaped response with the configured token usage,
// so the metering decorator charges a deterministic dollar amount per call. It
// touches no network (the whole point of the hermetic suite).
type fakeProvider struct {
	id    string
	usage model.Usage
	calls int
}

func (f *fakeProvider) Model() string { return f.id }

func (f *fakeProvider) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	f.calls++
	return model.Response{
		Content:    []model.Block{{Type: "text", Text: "ok"}},
		StopReason: "end_turn",
		Usage:      f.usage,
	}, nil
}

// denyApprover is a policy.Approver that always denies, so a test never blocks on a
// human gate and the promote path is exercised in its default-deny shape (I3).
type denyApprover struct{}

func (denyApprover) Approve(string) bool { return false }

// newGoRepo creates a temp git repo with a HEAD and a go.mod, so verify.Detect
// returns a real command (not the vacuous "true"). That makes project.NeedsBootstrap
// false, so buildStack takes the existing-repo path and never invokes the sandboxed
// greenfield scaffold (which would need a container) — keeping the test hermetic.
func newGoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "test@nilcore.local")
	git("config", "user.name", "nilcore-test")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/svc\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	return dir
}

// TestRegisterBuildFlagsDefaults asserts the build subcommand's flag surface and
// its §9 defaults, so a missing/renamed flag is caught here rather than at runtime.
func TestRegisterBuildFlagsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	bf := registerBuildFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if *bf.maxIter != 12 {
		t.Errorf("max-iterations default = %d, want 12", *bf.maxIter)
	}
	if *bf.maxFan != 8 {
		t.Errorf("max-fanout default = %d, want 8", *bf.maxFan)
	}
	if *bf.maxAgent != 64 {
		t.Errorf("max-agents default = %d, want 64", *bf.maxAgent)
	}
	if *bf.maxDepth != 1 {
		t.Errorf("max-depth default = %d, want 1", *bf.maxDepth)
	}
	if *bf.budget != 25.00 {
		t.Errorf("budget default = %v, want 25.00", *bf.budget)
	}
	if bf.deadline.String() != "2h0m0s" {
		t.Errorf("deadline default = %v, want 2h", *bf.deadline)
	}
	if *bf.goal != "" || *bf.dir != "" || *bf.fresh != "" {
		t.Errorf("goal/dir/new should default empty, got %q/%q/%q", *bf.goal, *bf.dir, *bf.fresh)
	}

	// -new and -dir must both be accepted as values (mutual exclusion is enforced in
	// buildMain, not by the flag set).
	fs2 := flag.NewFlagSet("build", flag.ContinueOnError)
	bf2 := registerBuildFlags(fs2)
	if err := fs2.Parse([]string{"-goal", "g", "-new", "./svc", "-budget", "3.5"}); err != nil {
		t.Fatalf("parse with args: %v", err)
	}
	if *bf2.goal != "g" || *bf2.fresh != "./svc" || *bf2.budget != 3.5 {
		t.Errorf("parsed flags wrong: goal=%q new=%q budget=%v", *bf2.goal, *bf2.fresh, *bf2.budget)
	}
}

// TestMeterProviderSharedLedgerIsTheWall proves the wiring's budget guarantee: the
// metered providers buildStack builds charge ONE shared ledger, and once the
// accumulated spend would breach the global ceiling the next Complete returns
// budget.ErrCeiling. This is the dead-budget blocker closed (design §7) — without
// the meter the ceiling would never be charged.
func TestMeterProviderSharedLedgerIsTheWall(t *testing.T) {
	ledger := budget.New()
	ledger.SetGlobalCeiling(0.01) // a tiny wall

	// Two metered providers over the SAME ledger (as buildStack wires strong+exec):
	// charges from either count against the one global ceiling.
	inner := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 0, OutputTokens: 1000}}
	strong := meterProvider(inner, ledger, "supervisor")
	worker := meterProvider(&fakeProvider{id: "claude-opus-4-8", usage: model.Usage{InputTokens: 0, OutputTokens: 1000}}, ledger, "worker")

	ctx := context.Background()
	// Opus output is $0.025/1k → 1000 output tokens = $0.025 > $0.01: the FIRST
	// charge already breaches the ceiling, so Complete returns ErrCeiling.
	if _, err := strong.Complete(ctx, "", nil, nil, 16); !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("first metered Complete: err = %v, want ErrCeiling", err)
	}
	// The shared ledger recorded nothing (a rejected charge is not recorded), and the
	// worker provider over the same ledger is likewise walled off.
	if _, err := worker.Complete(ctx, "", nil, nil, 16); !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("worker metered Complete over shared ledger: err = %v, want ErrCeiling", err)
	}

	// Sanity: a generous ceiling lets the same call through and records the spend, so
	// the wall is the ceiling, not the meter refusing everything.
	open := budget.New()
	open.SetGlobalCeiling(100)
	okProv := meterProvider(&fakeProvider{id: "claude-opus-4-8", usage: model.Usage{OutputTokens: 1000}}, open, "supervisor")
	if _, err := okProv.Complete(ctx, "", nil, nil, 16); err != nil {
		t.Fatalf("under-ceiling Complete: unexpected err %v", err)
	}
	if _, dollars := open.Total(); dollars <= 0 {
		t.Errorf("under-ceiling Complete should have charged the ledger, got $%v", dollars)
	}
}

// TestBuildStackBudgetCeilingAbortsRunaway assembles the full stack via buildStack
// (the real wiring) against a temp go-repo, injecting a shared ledger already at its
// ceiling. The project loop buildStack hands that ledger to must abort the run via
// the budget rail — terminating with ReasonBudget — rather than spinning to
// MaxIterations or reaching any container work. This is the runaway-abort guarantee
// the acceptance criterion names, exercised through the genuine build wiring.
func TestBuildStackBudgetCeilingAbortsRunaway(t *testing.T) {
	repo := newGoRepo(t)
	log := openTestLog(t)

	// A shared ledger pre-exhausted to its ceiling: the loop's per-iteration budget
	// probe (a negligible reservation) trips immediately, so the loop ends on
	// ReasonBudget before any model/spawn/integrate work — the same termination a
	// real metered runaway reaches once spend crosses the wall.
	ledger := budget.New()
	ledger.SetGlobalCeiling(0.0001)
	// Push the global total to the ceiling so any further charge is refused.
	if err := ledger.Charge(context.Background(), "preload", 0, 0.0001); err != nil {
		t.Fatalf("preload charge: %v", err)
	}

	prov := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{OutputTokens: 10}}
	stack, err := buildStack(buildDeps{
		goal:     "build a tiny service",
		dir:      repo,
		runtime:  "podman",
		image:    defaultBuildImage,
		maxIter:  12,
		maxFan:   8,
		maxAgent: 64,
		maxDepth: 1,
		maxSteps: 80,
		budget:   0, // ignored: an explicit ledger is injected below
		executor: prov,
		strong:   prov,
		log:      log,
		approver: denyApprover{},
		ledger:   ledger,
	})
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}

	// The wiring must hand the injected ledger straight through (the single-wall
	// invariant): the assembly's ledger is the one we exhausted.
	if stack.ledger != ledger {
		t.Fatal("buildStack did not thread the injected shared ledger through")
	}
	if stack.repo != repo {
		t.Errorf("stack.repo = %q, want %q (existing repo, no greenfield bootstrap)", stack.repo, repo)
	}

	out, err := stack.loop.Run(context.Background())
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if out.Reason != project.ReasonBudget {
		t.Fatalf("loop terminated with reason %q, want %q (budget wall)", out.Reason, project.ReasonBudget)
	}
	if out.Done {
		t.Error("a budget-aborted run must not report Done")
	}
	// No model call should have been needed to trip the wall (the probe trips first),
	// proving the rail fires before any spend — a runaway is cut off, not ridden out.
	if prov.calls != 0 {
		t.Errorf("expected 0 model calls before the budget rail fired, got %d", prov.calls)
	}
}

// openTestLog opens an append-only event log under the test's temp dir.
func openTestLog(t *testing.T) *eventlog.Log {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// _ keeps policy imported for the denyApprover's interface conformance check below
// (policy.Approver), making the intent explicit to a reader.
var _ policy.Approver = denyApprover{}
