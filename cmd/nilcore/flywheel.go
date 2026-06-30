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
// MEASURED-DELTA FENCE (SIF-T05, candidate-aware): the loop's regression fence now
// re-scores the frozen suite WITH the candidate edit actually applied — scoreFlywheelCandidate
// cuts a scratch worktree, runs the proposal there (KeepBranch), merges the verified edit,
// and scores the suite against that edited tree, so the fence reads a true before/after.
// It is FAIL-CLOSED: any error in that pipeline returns an empty report, which the fence
// reads as "no improvement" → the candidate is dropped (it can then only ever merge via the
// human gate in Propose, never auto). So a flaw in the scorer can only be CONSERVATIVE — the
// verifier + gate remain the sole ship authority regardless. The live behavior is the
// field-validation step (mirroring how the kernel/decompose recursive engine shipped:
// opt-in + hermetically tested at the seam + proven in the field).

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
	"nilcore/internal/flywheel/selfeval"
	"nilcore/internal/graapprove"
	"nilcore/internal/policy"
	"nilcore/internal/selfimprove"
	"nilcore/internal/worktree"
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
		report := eval.Run(ctx, cases, "flywheel", func(ctx context.Context, cse eval.Case) (bool, float64) {
			out, err := runViaKernel(ctx, orch, backend.Task{ID: fmt.Sprintf("flywheel-eval-%d", time.Now().UnixNano()), Goal: cse.Goal})
			if err != nil {
				return false, 0
			}
			return out.Verified, 0 // the verifier's verdict, never a self-report (I2)
		})
		// Closed-loop fold: record this baseline's verifier-judged self-eval pass-rate as a
		// durable selfeval_report event (selfeval.Fold gates on a verified chain — I5 — and
		// refuses a non-verifier-judged report — I2). trust.Replay folds it into the
		// per-config EVIDENCE view so `nilcore trust` shows which config earned its standing;
		// it never feeds routing. nil ledger ⇒ no transient in-memory fold (the durable
		// record is the event). Best-effort behind the authoritative run: a fold error
		// (e.g. no log yet) never fails the suite. Skipped when there is no log to verify.
		if log != nil {
			_, _ = selfeval.Fold(ctx, logPath, selfeval.NewVerifierJudged(report), nil, log)
		}
		return report, nil
	}
	flow := &selfimprove.Flow{
		Scope: selfimprove.DefaultScope(),
		Run: func(ctx context.Context, g string) (bool, error) {
			out, err := runViaKernel(ctx, orch, backend.Task{ID: fmt.Sprintf("flywheel-edit-%d", time.Now().UnixNano()), Goal: g})
			if err != nil {
				return false, err
			}
			return out.Verified, nil
		},
		Gate: graapprove.SelfImproveGate(policy.NewConsoleApprover(os.Stdin, os.Stdout).Approve, autoApproveSink{log}),
		Log:  log,
	}
	return loop.New(loop.Config{
		LogPath:  logPath,
		RunSuite: runSuite,
		// Candidate-aware "after" score: apply the proposal in a scratch worktree and
		// score the suite against the edited tree (a true before/after). Fail-closed —
		// any error ⇒ empty report ⇒ the fence drops the candidate (never auto-accepts).
		ScoreCandidate: func(ctx context.Context, cases []eval.Case, prop selfimprove.Proposal) (eval.Report, error) {
			rep, err := scoreFlywheelCandidate(ctx, orch, cases, prop)
			if err != nil {
				log.Append(eventlog.Event{Kind: "flywheel_candidate_score_failed",
					Detail: map[string]any{"goal": prop.Goal, "error": err.Error()}})
				return eval.Report{}, nil // fail closed: no measured gain ⇒ candidate dropped
			}
			return rep, nil
		},
		Propose:       flow.Propose,
		MaxIterations: maxIter,
		Interval:      interval,
	})
}

// scoreFlywheelCandidate measures a self-improvement proposal's ACTUAL effect: it cuts a
// scratch worktree off the agent's repo, runs the proposal there (KeepBranch → a verified
// edit branch), merges that branch, and scores the frozen self-eval suite against the
// edited tree. The returned report is the candidate's pass-rate, which the loop's fence
// compares to the baseline. Every failure mode (worktree, unverified edit, merge conflict)
// returns an error the caller turns into a fail-closed "no gain" — so a buggy scorer can
// only ever be conservative, never accept a non-improving edit (I2: the verifier still
// governs each run; this only ORDERS whether to pursue the merge).
//
// NOTE — this is a MEASURE step, deliberately NOT an integration loop, so it does NOT use
// internal/integrate.Integrator (the canonical merge→re-verify→rollback engine the build,
// swarm, and decompose paths share). The Integrator verifies the merged tip and ROLLS BACK
// a red merge; here we want the single verified edit to STAY merged so we can SCORE the
// eval suite against it (success is a higher eval pass-rate, not the project verifier's
// verdict on the merge). It is a throwaway measurement — nothing ships — so it correctly
// emits no integration_* audit events; it reuses the shared worktree.Merge primitive, not
// a second integration engine.
func scoreFlywheelCandidate(ctx context.Context, orch *agent.Orchestrator, cases []eval.Case, prop selfimprove.Proposal) (eval.Report, error) {
	leaf := fmt.Sprintf("fw-cand-%d", time.Now().UnixNano())
	wt, err := worktree.CreateFrom(ctx, orch.BaseRepo, "flywheel-cand/"+leaf, leaf, "HEAD")
	if err != nil {
		return eval.Report{}, fmt.Errorf("scratch worktree: %w", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// Apply the candidate edit IN the scratch worktree (a clone of the orchestrator bound
	// to the scratch, KeepBranch so the verified edit's branch is preserved to merge).
	editOrch := *orch
	editOrch.BaseRepo = wt.Path()
	editOrch.KeepBranch = true
	out, err := runViaKernel(ctx, &editOrch, backend.Task{ID: "fw-edit-" + leaf, Goal: prop.Goal})
	if err != nil {
		return eval.Report{}, fmt.Errorf("apply candidate: %w", err)
	}
	if !out.Verified || out.Branch == "" {
		// The edit itself did not verify ⇒ it cannot be an improvement; no gain.
		return eval.Report{}, nil
	}
	if conflict, merr := wt.Merge(ctx, out.Branch, "flywheel: measure candidate"); merr != nil || conflict {
		return eval.Report{}, fmt.Errorf("merge candidate (conflict=%v): %w", conflict, merr)
	}

	// Score the frozen suite against the merged (edited) scratch tree.
	scoreOrch := *orch
	scoreOrch.BaseRepo = wt.Path()
	return eval.Run(ctx, cases, "flywheel-candidate", func(ctx context.Context, cse eval.Case) (bool, float64) {
		o, rerr := runViaKernel(ctx, &scoreOrch, backend.Task{ID: fmt.Sprintf("fw-cand-eval-%d", time.Now().UnixNano()), Goal: cse.Goal})
		if rerr != nil {
			return false, 0
		}
		return o.Verified, 0
	}), nil
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
