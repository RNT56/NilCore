package main

// flywheel.go is the cmd-side wiring for the self-improvement flywheel (Phase 16,
// Pillar 4 / SIF-T08): the operator face `nilcore flywheel` and the optional serve
// background cadence. The flywheel scores the content-hash-FROZEN self-eval suite,
// mines RECURRING verifier-failure scars from the append-only log, and proposes a
// measured-improving remediation through the GATED self-improve flow. It NEVER ships
// an edit itself and NEVER bypasses the verifier (I2) or the human gate; it never
// edits the verifier of record (selfimprove.DefaultScope deny). With
// NILCORE_SELFIMPROVE_AUTOAPPROVE set (the SEPARATE double opt-in — SIF-T07) the merge
// gate auto-approves a verifier-green, measured-improving self-edit; otherwise it asks.
//
// HONEST LIMITATION (recorded in docs/ROADMAP-SELF-IMPROVEMENT.md): the loop's
// within-cycle regression fence (measure.Fence) re-scores the frozen suite via the
// SAME injected RunSuite for both the baseline and the candidate, so it does not yet
// re-score WITH the candidate edit applied — candidate-aware re-scoring is a tracked
// refinement. Until then the conservative fence rarely accepts, so the flywheel surfaces
// scars and proposes only when a measured gain is observed; the verifier + gate remain
// the sole ship authority regardless.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"nilcore/eval"
	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/flywheel/loop"
	"nilcore/internal/graapprove"
	"nilcore/internal/policy"
	"nilcore/internal/selfimprove"
)

// serveFlywheelInterval is the (conservative) cadence of the optional serve-mode
// flywheel: a cycle scores the eval suite, which is not cheap, so it runs rarely.
const serveFlywheelInterval = 6 * time.Hour

// flywheelMain implements `nilcore flywheel` — run a bounded flywheel cadence and
// print a structural summary. It is an explicit operator command (no env gate to run
// it); the heavy work (scoring the suite, running edits) is the operator's choice.
func flywheelMain(args []string) {
	fs := flag.NewFlagSet("flywheel", flag.ExitOnError)
	once := fs.Bool("once", false, "run exactly one bounded cycle and exit")
	iterations := fs.Int("iterations", 1, "max cycles per run (a bounded standing cadence)")
	interval := fs.Duration("interval", time.Minute, "throttle between cycles")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))
	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()

	maxIter := *iterations
	if *once {
		maxIter = 1
	}
	orch := buildRunOrchestrator(c, b, log, absDir, mintBlastBudget(*c.blastRadius, log))
	fw := newFlywheelLoop(orch, log, *c.logPath, maxIter, *interval)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	sum, err := fw.Run(ctx)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("flywheel: %d cycle(s) · %d candidate(s) · %d proposed · %d accepted · %d merged · %d skipped\n",
		sum.Iterations, sum.Candidates, sum.Proposed, sum.Accepted, sum.Merged, sum.Skipped)
}

// newFlywheelLoop wires the standing loop over an orchestrator. RunSuite scores the
// frozen suite by driving the orchestrator per case (the VERIFIER decides pass/fail,
// so the pass-rate is verifier-judged — I2). Propose routes a candidate through the
// gated selfimprove flow whose merge gate is the SelfImproveGate (human by default,
// auto only under NILCORE_SELFIMPROVE_AUTOAPPROVE — SIF-T07), and whose Scope denies
// the verifier of record and every contract file. Constructing the loop does NOTHING;
// only Run drives a cycle (default-off).
func newFlywheelLoop(orch *agent.Orchestrator, log *eventlog.Log, logPath string, maxIter int, interval time.Duration) *loop.Loop {
	runSuite := func(ctx context.Context, cases []eval.Case) (eval.Report, error) {
		return eval.Run(ctx, cases, "flywheel", func(ctx context.Context, cse eval.Case) (bool, float64) {
			out, err := orch.Execute(ctx, backend.Task{ID: fmt.Sprintf("flywheel-eval-%d", time.Now().UnixNano()), Goal: cse.Goal})
			if err != nil {
				return false, 0
			}
			return out.Verified, 0 // the verifier's verdict, never a self-report (I2)
		}), nil
	}
	flow := &selfimprove.Flow{
		Scope: selfimprove.DefaultScope(),
		Run: func(ctx context.Context, g string) (bool, error) {
			out, err := orch.Execute(ctx, backend.Task{ID: fmt.Sprintf("flywheel-edit-%d", time.Now().UnixNano()), Goal: g})
			if err != nil {
				return false, err
			}
			return out.Verified, nil
		},
		Gate: graapprove.SelfImproveGate(policy.NewConsoleApprover(os.Stdin, os.Stdout).Approve, autoApproveSink{log}),
		Log:  log,
	}
	return loop.New(loop.Config{
		LogPath:       logPath,
		RunSuite:      runSuite,
		Propose:       flow.Propose,
		MaxIterations: maxIter,
		Interval:      interval,
	})
}

// runFlywheelTicker drives a pre-built flywheel loop as a bounded serve-background
// cadence (one cycle per tick, serveFlywheelInterval apart). It honors ctx so serve
// shutdown stops it promptly. The default-off gate (NILCORE_FLYWHEEL) is checked by
// the caller in serveMain, where the orchestrator is built at startup so a missing
// model key fails loudly at boot rather than inside this goroutine.
func runFlywheelTicker(ctx context.Context, fw *loop.Loop) {
	t := time.NewTicker(serveFlywheelInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := fw.Run(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "nilcore: flywheel cycle: %v\n", err)
			}
		}
	}
}
