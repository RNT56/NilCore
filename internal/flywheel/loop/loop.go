// Package loop is the self-improvement flywheel's STANDING LOOP (Phase 16,
// docs/ROADMAP-CLOSED-LOOP.md §4 Pillar 4, SIF-T06). It is the bounded cadence
// driver that composes the four flywheel leaves into one repeatable cycle:
//
//	(1) BASELINE  — run the content-hash-FROZEN self-eval suite (eval/self) on
//	    the agent to a baseline eval.Report (selfeval establishes the
//	    verifier-judged guarantee at the harness boundary).
//	(2) DISTILL   — mine the append-only event log for RECURRING verifier-failure
//	    patterns (distiller), turning the agent's own scars into candidate
//	    improvement TARGETS.
//	(3) FENCE     — for a candidate, accept it ONLY if measure.Improved(before,
//	    after) holds: a candidate must MEASURABLY raise pass-rate over the frozen
//	    suite, never on a hunch (the C6 regression fence).
//	(4) PROPOSE   — route the candidate through selfimprove.Flow, which STILL
//	    requires the verifier (I2) and the human gate. This loop NEVER ships an
//	    edit itself and NEVER bypasses either; it only decides WHICH target to
//	    propose, and the fence decides whether to keep it.
//
// Safety stance (the whole point — see the roadmap §4 and CLAUDE.md §2):
//
//   - I2 (the verifier is the SOLE authority on "done"). The loop folds nothing
//     and ships nothing on its own. The baseline/candidate pass-rates come from
//     selfeval's verifier-judged reports; the keep/drop decision is measure's
//     measured delta; the actual edit only merges if selfimprove's verifier is
//     green AND the human gate approves. The loop is pure orchestration over
//     those gates — it can only ever DELAY or SKIP a proposal, never force a
//     ship.
//   - C6 feedback-loop pathologies are structurally guarded:
//     · The eval set is NEVER mutated. The loop loads the FROZEN suite from
//     eval/self (a defensive copy, content-hashed) and re-uses the same suite
//     for the baseline and every candidate measurement, so a candidate cannot
//     drop the cases it fails. The loop never writes to eval/self.
//     · The verifier-of-record is NEVER self-modified. The loop refuses to
//     propose any target whose remediation would touch the frozen verify
//     package or any other denied path: it relies on selfimprove.Scope's
//     deny-list (DefaultScope denies internal/verify/) AND additionally
//     screens every proposal's paths up front, so a target aimed at the
//     verifier is dropped before it is ever run.
//   - The cadence is BOUNDED: MaxIterations caps how many cycles a single Run
//     performs and Interval throttles them, so a runaway loop is impossible.
//
// DEFAULT-OFF: constructing a Loop does NOTHING on its own — no eval runs, no
// log is read, no proposal is made. Only an explicit Run(ctx) (which the cmd
// layer calls later under NILCORE_FLYWHEEL, SIF-T08) drives a cycle. An unwired
// flywheel is byte-identical to today.
//
// This package is a WIRING leaf for the flywheel: it imports the flywheel
// leaves (selfeval/distiller/measure), the frozen suite (eval/self), the eval
// harness, and selfimprove (the gated flow it proposes through). It imports NO
// orchestrator/cmd package — the cmd layer wires the loop, never the reverse —
// and takes the actual eval-run and propose as INJECTED funcs, so tests need no
// model and no network. deps_test.go enforces that closure.
package loop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nilcore/eval"
	evalself "nilcore/eval/self"
	"nilcore/internal/flywheel/distiller"
	"nilcore/internal/flywheel/measure"
	"nilcore/internal/selfimprove"
)

// ErrNoRunner / ErrNoPropose / ErrNoLogPath are returned by Run when a required
// injected dependency is missing. They are distinct, errors.Is-matchable
// sentinels so a caller can tell a misconfiguration apart from a cycle that
// simply found nothing to do (which is NOT an error — an idle agent with no
// recurring scars is the healthy steady state).
var (
	ErrNoRunner  = errors.New("flywheel/loop: no eval Runner injected (the loop needs a way to score the frozen suite)")
	ErrNoPropose = errors.New("flywheel/loop: no Propose func injected (the loop needs the gated selfimprove flow)")
	ErrNoLogPath = errors.New("flywheel/loop: empty event-log path (the distiller needs a chain to verify, fail-closed)")
)

// ProposeFunc is the gated self-edit flow the loop proposes a candidate through.
// In production it is a thin closure over (*selfimprove.Flow).Propose — which
// runs the candidate as a verified task and requires the human gate before any
// merge (I2 + the gate). It is INJECTED so a test can assert the loop's
// decisions without a worktree, a verifier, or a human: the loop only decides
// WHETHER and WHICH to propose; this func owns the actual verify+gate.
//
// merged reports whether the edit actually shipped (verifier-green AND
// human-approved). The measured-delta fence already ran at THIS loop level
// (step 3 below) before Propose is ever called, so the gated flow it wraps owns
// only the verifier + human gate. The loop treats a false merged as a perfectly
// normal outcome (the human declined, or the verifier failed): it is NOT an
// error, and the loop continues its bounded cadence.
type ProposeFunc func(ctx context.Context, p selfimprove.Proposal) (merged bool, err error)

// RunSuiteFunc scores the FROZEN self-eval suite under the agent and returns the
// resulting eval.Report. It is INJECTED so the loop needs no model or network in
// a test: production passes a closure that drives the orchestrator against the
// suite via eval.Run (whose Runner returns the VERIFIER's pass/fail, so the
// pass-rate the loop measures is verifier-judged, I2). The loop hands it the
// frozen, content-hashed cases (which it never mutates) so the same yardstick
// scores the baseline and every candidate.
type RunSuiteFunc func(ctx context.Context, cases []eval.Case) (eval.Report, error)

// Config holds the loop's BOUNDED cadence and its (injected) dependencies. The
// zero value is inert: New applies safe defaults and Run validates the required
// injected funcs, so a half-configured loop fails closed rather than running
// unbounded or unverified.
type Config struct {
	// LogPath is the append-only event log the distiller replays to mine
	// recurring verifier-failure targets. REQUIRED: an empty path is refused
	// (the distiller must have a chain to verify, fail-closed — I5).
	LogPath string

	// RotatedLogPaths are PRIOR log generations (e.g. the maint.RotateLog output
	// LogPath+".1") the distiller should ALSO replay, so a recurring scar that
	// straddles a rotation boundary still clears the recurrence threshold instead
	// of resetting its Count when the live log was rotated to a fresh genesis
	// chain (the B5-autonomy.8 fix). Each generation is its own hash chain, so the
	// distiller chain-verifies each independently and fails closed per file. Empty
	// or missing generations are skipped cleanly; an empty slice (the zero value)
	// is byte-identical to mining only LogPath.
	RotatedLogPaths []string

	// RunSuite scores the frozen self-eval suite (injected; REQUIRED). See
	// RunSuiteFunc.
	RunSuite RunSuiteFunc

	// Propose routes a candidate through the gated selfimprove flow (injected;
	// REQUIRED). See ProposeFunc.
	Propose ProposeFunc

	// PlanTarget maps a distilled failure pattern to a concrete self-improvement
	// Proposal (which prompt/skill to edit, the goal, the reason). It is injected
	// because authoring the remediation is a model-shaped step the loop must not
	// hard-code; a nil PlanTarget uses defaultPlan, a deterministic, model-free
	// mapping that names the target structurally (never quoting raw scar text,
	// I7) and touches only an allow-listed prompt path. Returning a zero-Goal
	// Proposal (or ok=false) means "no actionable target here" and the candidate
	// is skipped — never proposed.
	PlanTarget func(p distiller.Pattern) (prop selfimprove.Proposal, ok bool)

	// Scope is the self-edit allow/deny surface every proposal is pre-screened
	// against BEFORE it is run, so a target aimed at a denied path (the verifier
	// of record, the core loop, a contract file) is dropped here as well as by
	// selfimprove. The zero value uses selfimprove.DefaultScope.
	Scope selfimprove.Scope

	// Fence is the regression fence (measure). A candidate is accepted only if it
	// MEASURABLY improves the frozen-suite pass-rate over the baseline. The zero
	// value is the maximally-conservative fence (a strict improvement; a tie or
	// regression never passes).
	Fence measure.Fence

	// ScoreCandidate, when set, scores the frozen suite AGAINST THE CANDIDATE — it
	// applies the proposal in a scratch worktree and re-runs the suite there, so the
	// fence's "after" report reflects the proposed edit's actual effect (the true
	// before/after the measured-delta guarantee requires). When nil the "after" score
	// falls back to RunSuite over the current state — BYTE-IDENTICAL to the prior
	// behavior, which is why the loop's own tests (whose fake RunSuite simulates the
	// candidate effect) are unaffected. Production wires this to a real scratch-worktree
	// scorer; the deterministic test suites leave it nil.
	ScoreCandidate func(ctx context.Context, cases []eval.Case, prop selfimprove.Proposal) (eval.Report, error)

	// MaxIterations bounds how many cycles ONE Run performs (the hard cap that
	// makes a runaway loop impossible). New defaults a non-positive value to
	// DefaultMaxIterations.
	MaxIterations int

	// Interval throttles successive cycles within a single Run. New defaults a
	// non-positive value to DefaultInterval. The loop honors ctx during the
	// wait, so a cancelled context stops it promptly.
	Interval time.Duration

	// DistillThreshold is the minimum recurrence (Count) for a failure pattern to
	// become a candidate target; <= 0 uses distiller.DefaultThreshold (so a
	// one-off scar is never an improvement target).
	DistillThreshold int

	// MaxProposalsPerIter bounds how many candidate targets one cycle proposes,
	// so a log full of distinct scars cannot fan out into an unbounded run of
	// proposals. New defaults a non-positive value to DefaultMaxProposalsPerIter.
	MaxProposalsPerIter int

	// now is an injected clock for deterministic tests; nil uses time.Now.
	now func() time.Time
}

// Cadence defaults — conservative bounds so an unconfigured loop is still safe.
const (
	// DefaultMaxIterations caps cycles per Run. One pass is the safe default; a
	// caller that wants a standing cadence sets a higher cap and a real Interval.
	DefaultMaxIterations = 1
	// DefaultInterval throttles cycles. A non-trivial wall so a multi-iteration
	// loop cannot busy-spin.
	DefaultInterval = time.Minute
	// DefaultMaxProposalsPerIter caps proposals per cycle.
	DefaultMaxProposalsPerIter = 1
)

// Loop is the constructed, ready-but-inert standing loop. Constructing it does
// NOTHING (default-off); only Run drives a cycle.
type Loop struct {
	cfg Config
}

// New builds a Loop from cfg, applying safe defaults to the cadence and the
// fence/scope/plan seams. It performs NO IO and starts NOTHING — it only
// validates and normalizes configuration. A nil-required-func is NOT rejected
// here (so New never fails for a partially-wired loop); Run fails closed on a
// missing required dependency instead, with a precise sentinel.
func New(cfg Config) *Loop {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxProposalsPerIter <= 0 {
		cfg.MaxProposalsPerIter = DefaultMaxProposalsPerIter
	}
	if cfg.DistillThreshold <= 0 {
		cfg.DistillThreshold = distiller.DefaultThreshold
	}
	if len(cfg.Scope.Allow) == 0 && len(cfg.Scope.Deny) == 0 {
		cfg.Scope = selfimprove.DefaultScope()
	}
	if cfg.PlanTarget == nil {
		cfg.PlanTarget = defaultPlan
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Loop{cfg: cfg}
}

// Summary is the structural, auditable result of a Run — counts only, never raw
// scar text (I7), never a secret (I3). It lets a caller log/trace what the loop
// did without re-deriving it.
type Summary struct {
	Iterations int // cycles actually performed (<= MaxIterations)
	Candidates int // distilled improvement targets considered across all cycles
	Proposed   int // candidates routed through the gated flow
	Accepted   int // candidates the regression fence kept (cleared the margin)
	Merged     int // candidates the gated flow actually shipped (verifier + human)
	Skipped    int // candidates dropped (no plan, out of scope, or fence rejected)
}

// Run drives the bounded standing loop until MaxIterations is reached, the
// context is cancelled, or a cycle finds no actionable target. It returns a
// structural Summary of what happened.
//
// Each cycle:
//
//  1. scores the FROZEN self-eval suite to a baseline report (RunSuite over
//     eval/self's content-hashed cases — never a mutated set, C6);
//  2. distills recurring verifier-failure targets from the event log
//     (fail-closed on a broken chain — I5);
//  3. for each target (up to MaxProposalsPerIter): plans a remediation, screens
//     it against the self-edit scope (dropping any target aimed at the verifier
//     of record or another denied path, C6), re-scores the suite WITH the
//     candidate, and accepts it ONLY if the fence says it measurably improved;
//  4. proposes an accepted candidate through the gated flow (verifier + human
//     gate own the ship decision — this loop never bypasses them, I2).
//
// Run validates its required injected dependencies first and fails closed on
// any missing one (a precise sentinel). A cycle that finds no recurring scar is
// a normal, non-error early stop (the healthy steady state).
func (l *Loop) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	if l == nil {
		return sum, nil
	}
	if l.cfg.RunSuite == nil {
		return sum, ErrNoRunner
	}
	if l.cfg.Propose == nil {
		return sum, ErrNoPropose
	}
	if l.cfg.LogPath == "" {
		return sum, ErrNoLogPath
	}

	for i := 0; i < l.cfg.MaxIterations; i++ {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		// Throttle between cycles (never before the first), honoring ctx. The
		// bounded wall makes a multi-iteration loop impossible to busy-spin.
		if i > 0 {
			if err := sleep(ctx, l.cfg.Interval); err != nil {
				return sum, err
			}
		}

		more, err := l.cycle(ctx, &sum)
		if err != nil {
			return sum, err
		}
		sum.Iterations++
		if !more {
			// No actionable target this cycle ⇒ stop early. An idle agent with no
			// recurring scars has nothing to improve — that is success, not failure.
			break
		}
	}
	return sum, nil
}

// cycle performs one baseline → distill → fence → propose pass. It returns
// more=true when there was at least one candidate target to consider (so the
// loop may keep iterating) and more=false when the log held no recurring scar
// (so the loop stops early). It mutates sum in place with the structural counts.
func (l *Loop) cycle(ctx context.Context, sum *Summary) (more bool, err error) {
	// (1) BASELINE: score the FROZEN suite. Loading from eval/self returns a
	// defensive copy of the content-hashed cases; the loop never mutates them, so
	// the same yardstick measures the baseline and every candidate (C6: no
	// eval-set self-modification).
	suite, _, err := evalself.Load()
	if err != nil {
		return false, fmt.Errorf("loading frozen self-eval suite: %w", err)
	}
	baseline, err := l.cfg.RunSuite(ctx, suite.Cases)
	if err != nil {
		return false, fmt.Errorf("running baseline self-eval: %w", err)
	}

	// (2) DISTILL: mine recurring verifier-failure targets from the event log AND
	// any prior rotated generations, so a scar that straddles a log rotation still
	// clusters (B5-autonomy.8). Fail-closed per generation on a broken chain
	// (DistillAcross chain-verifies each file and returns an error + nil patterns
	// over a tampered one — I5), so the loop earns no target from forged evidence.
	gens := append([]string{l.cfg.LogPath}, l.cfg.RotatedLogPaths...)
	patterns, err := distiller.DistillAcross(l.cfg.DistillThreshold, gens...)
	if err != nil {
		return false, fmt.Errorf("distilling improvement targets: %w", err)
	}
	if len(patterns) == 0 {
		return false, nil // nothing recurring to improve — stop the cadence early
	}

	// (3)+(4) FENCE + PROPOSE for up to MaxProposalsPerIter targets, strongest
	// recurrence first (distiller already sorts by descending Count).
	proposed := 0
	for _, pat := range patterns {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if proposed >= l.cfg.MaxProposalsPerIter {
			break
		}
		sum.Candidates++

		// Plan a concrete remediation for this structural target. A nil/zero plan
		// means "nothing actionable here" — skip, never propose.
		prop, ok := l.cfg.PlanTarget(pat)
		if !ok || prop.Goal == "" || len(prop.Paths) == 0 {
			sum.Skipped++
			continue
		}

		// C6 + I2 screen: a target aimed at the verifier of record (or any other
		// denied path) is DROPPED before it is ever run. selfimprove's own scope
		// check would also reject it, but screening here keeps the verifier
		// untouchable even if a future caller injects a permissive Propose.
		if inScope, _ := l.cfg.Scope.Check(prop); !inScope {
			sum.Skipped++
			continue
		}

		// FENCE: re-score the FROZEN suite WITH the candidate's effect and accept it
		// ONLY if it measurably improved (measure). When ScoreCandidate is wired, the
		// "after" report is produced by APPLYING the proposal in a scratch worktree and
		// re-running the suite there — a true before/after; the fence reads the verifier-
		// judged pass-rate delta, never a self-claim (I2). When it is nil, "after" falls
		// back to RunSuite over the current state (byte-identical to before). A tie or
		// regression is rejected (C6).
		var after eval.Report
		if l.cfg.ScoreCandidate != nil {
			after, err = l.cfg.ScoreCandidate(ctx, suite.Cases, prop)
		} else {
			after, err = l.cfg.RunSuite(ctx, suite.Cases)
		}
		if err != nil {
			return true, fmt.Errorf("running candidate self-eval: %w", err)
		}
		if !l.cfg.Fence.Improved(baseline, after) {
			sum.Skipped++
			continue // no measured improvement — drop the candidate, never propose
		}
		sum.Accepted++

		// PROPOSE through the gated flow. This is the ONLY ship path and it STILL
		// requires the verifier (I2) and the human gate inside Propose — the loop
		// never bypasses either. A declined/unverified candidate returns
		// merged=false, which is a normal outcome, not an error.
		merged, err := l.cfg.Propose(ctx, prop)
		if err != nil {
			return true, fmt.Errorf("proposing self-improvement: %w", err)
		}
		proposed++
		sum.Proposed++
		if merged {
			sum.Merged++
		}
	}
	return true, nil
}

// defaultPlan maps a distilled failure pattern to a deterministic, model-free
// self-improvement Proposal. It names the target STRUCTURALLY — by the verifier
// id and coarse failure class the distiller already extracted — and NEVER quotes
// raw scar text (I7). It targets a single allow-listed prompt path
// (docs/PERSONA.md, which DefaultScope permits and which is NOT the verifier of
// record), so the default proposal is in scope by construction. A real
// deployment injects a smarter PlanTarget; this keeps the loop runnable and
// safe with no wiring.
func defaultPlan(p distiller.Pattern) (selfimprove.Proposal, bool) {
	if p.VerifierID == "" {
		return selfimprove.Proposal{}, false
	}
	return selfimprove.Proposal{
		Reason: fmt.Sprintf("recurring verifier failure: verifier=%s class=%s count=%d", p.VerifierID, p.FailClass, p.Count),
		Paths:  []string{"docs/PERSONA.md"},
		Goal:   fmt.Sprintf("Improve the agent's guidance to reduce the recurring %s/%s verifier failure.", p.VerifierID, p.FailClass),
	}, true
}

// sleep waits d honoring ctx — it returns ctx.Err() if the context is cancelled
// before d elapses, so a cancelled Run stops promptly rather than blocking out
// the full interval.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
