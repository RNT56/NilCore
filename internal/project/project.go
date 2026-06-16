// Package project is NilCore's outer loop (docs/MULTI-AGENT.md §5): from one
// high-level goal it drives plan → slice → integrate → verify → reflect → re-plan
// to convergence. It is deliberately MECHANICAL with PROVABLE termination — all
// the agentic reasoning lives in the supervisor it drives through the RunSlice
// seam (the same separation backend.Native uses: the loop is bounded plumbing,
// the model is the engine).
//
// Three properties are load-bearing and tested:
//
//   - Done is the VERIFIER's verdict, never an LLM's (I2). JudgeProject is an
//     exit-code AND over the project VerifyCmd and EVERY acceptance Criterion
//     command. A model's "looks done" prose can never flip Done to true.
//   - Termination rests on multiple INDEPENDENT ceilings, each a DISTINCT
//     Outcome.Reason: MaxIterations, MaxNoProgress, the budget Ledger ErrCeiling,
//     the wall-clock Deadline / ctx, and done-detection. The loop cannot spin: the
//     iteration counter is monotonic and every ceiling halts it.
//   - Recovery is a LADDER, never an abort (reflect.go): narrow → switch →
//     stop-and-ask the human. Partial slices keep their already-merged verified
//     work (the integrator guarantees the merged subset is green), so a stuck run
//     degrades to "the best green tree so far + a human question", never a crash.
//
// Carry-over between iterations is BOUNDED STATE only — a summarize.ContextSummary
// plus blackboard/memory facts the wiring layer threads in — never raw transcripts
// (the same context-bounding discipline summarize enforces everywhere else).
package project

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nilcore/internal/advisor"
	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/route"
	"nilcore/internal/summarize"
	"nilcore/internal/verify"
)

// ChannelAsk is the narrow human-in-the-loop seam the recovery ladder's final rung
// uses to stop-and-ask. It mirrors channel.Authorized.Ask (a yes/no question on a
// thread) without importing channel, keeping project a leaf the wiring site adapts
// (CLAUDE.md §4: leaf packages must not import the orchestrator). A nil Channel
// makes stop-and-ask a clean terminal Outcome rather than a panic — no ambient
// authority is assumed (I3).
type ChannelAsk interface {
	Ask(ctx context.Context, threadID, question string) (bool, error)
}

// Criterion is one acceptance check the project must satisfy. Command is a shell
// command whose exit code (run in the sandbox via Verifier) is the ONLY signal
// that gates done-ness — Description is human-readable data, never consulted by
// JudgeProject. A criterion with an empty Command is "covered by the project
// VerifyCmd" and contributes no independent gate (it is dropped at derivation).
type Criterion struct {
	Description string          // human-readable intent (data, never gates)
	Command     string          // exit-0 ⟺ met; the sole authority for this criterion
	Verifier    verify.Verifier // runs Command in the sandbox; resolved at DeriveAcceptance
}

// Slice is one bounded unit of work the supervisor will plan and run: a focused
// goal plus the bounded carry-over context. A Slice is NOT a transcript — it is
// the minimal handoff (summarize.ContextSummary) that seeds the supervisor.
type Slice struct {
	Goal    string
	Summary summarize.ContextSummary
}

// SliceResult is what RunSlice reports after the supervisor spawns + the
// integrator merges one slice into a single verified subtree. Branch is the
// integration tip; Verified is the integrator's verdict on that tip; Summary folds
// the slice's outcome back as bounded state (never a transcript). It mirrors the
// shape of super.Outcome so the wiring adapter is a thin field copy.
type SliceResult struct {
	Branch   string                   // integration tip this slice converged on
	Verified bool                     // the integrator re-verified this tip green
	Summary  summarize.ContextSummary // bounded fold-back for the next iteration
	Note     string                   // optional human-readable note (data only)
}

// State is the bounded carry-over the loop threads across iterations. It holds NO
// transcripts: just the goal, the chosen verify command, the derived acceptance
// criteria, the current integration tip, and a rolling ContextSummary. Everything
// here survives a context window because it is summary-shaped by construction.
type State struct {
	Goal      string                   // the high-level project goal
	Repo      string                   // the repo dir under work
	VerifyCmd string                   // the project's "done" command (verify.Detect or override)
	Criteria  []Criterion              // derived acceptance criteria (add-only refined)
	Branch    string                   // current integration tip
	Summary   summarize.ContextSummary // bounded rolling context (never a transcript)
	Iteration int                      // monotonic; the primary termination witness
}

// Loop is the bounded outer loop. The zero value is unusable; set at least Goal,
// Log, Plan, RunSlice, and Verifier. Every other field is an optional seam the
// wiring site fills — a nil seam degrades that capability gracefully (the loop
// stays bounded and never panics). The func seams (Plan/RunSlice/Verifier/Gate)
// are where the supervisor's agentic reasoning plugs in; this struct owns only the
// mechanical, provably-terminating control flow around them.
type Loop struct {
	Goal, Repo string
	Log        *eventlog.Log

	// Plan asks the supervisor for the next slice given bounded state. An error is
	// a recoverable signal (the reflect ladder narrows/switches), not a crash.
	Plan func(ctx context.Context, goal string, st State) (Slice, error)
	// RunSlice has the supervisor spawn + the integrator merge one slice into a
	// single verified subtree. Verified==false is a normal result the loop reflects
	// on, not an error.
	RunSlice func(ctx context.Context, sl Slice, st State) (SliceResult, error)
	// Verifier builds the project verifier for a directory (the same factory shape
	// the orchestrator uses). JudgeProject runs it plus each Criterion command.
	Verifier func(dir string) verify.Verifier

	Advisor  *advisor.Advisor                    // strong-tier reasoning for the reflect ladder
	Reviewer model.Provider                      // cross-model review before a promote (optional)
	Differ   func(branch string) (string, error) // produces the promote diff for Reviewer (optional; nil ⇒ no review)
	Gate     func(a policy.GateAction) bool      // the single gated, irreversible promote
	Channel  ChannelAsk                          // human stop-and-ask (recovery ladder's last rung)

	MaxIterations int // outer-loop iteration ceiling; <1 → a generous default
	MaxNoProgress int // consecutive no-progress iterations before stop-ask; <1 → default

	Budget   *budget.Ledger // shared ledger; ErrCeiling is the budget termination rail
	Deadline time.Time      // wall-clock ceiling; zero → no wall-clock rail

	// seeded is the acceptance bar the wiring/bootstrap derived (via DeriveAcceptance)
	// and handed to the loop with SeedCriteria BEFORE Run. It is private bookkeeping,
	// not configuration: the loop folds it into State.Criteria at start so JudgeProject
	// gates on it from iteration 0. Empty is valid — done then rests on the project
	// VerifyCmd alone (a project with no extra criteria converges on a green VerifyCmd).
	seeded []Criterion
}

// SeedCriteria installs the acceptance criteria the wiring derived (DeriveAcceptance)
// before Run. It is the seam by which the add-only, sandbox-vetted bar reaches the
// loop without widening the frozen configuration surface: derivation needs a sandbox
// the loop deliberately does not own, so the wiring derives and seeds. Calling it
// after Run has started has no effect (the loop snapshots criteria into State at
// start). Passing nil leaves the bar as the project VerifyCmd alone.
func (l *Loop) SeedCriteria(criteria []Criterion) {
	l.seeded = append([]Criterion(nil), criteria...)
}

// Outcome is the loop's terminal report. Done is the JudgeProject verdict (the
// project verifier AND every criterion command exited 0) — NEVER an LLM verdict.
// Reason names which ceiling or condition ended the run so a caller can branch on
// it; the set is closed (see the Reason* constants).
type Outcome struct {
	Done       bool   // JudgeProject passed: verifier AND every criterion exit 0
	Reason     string // one of the Reason* constants below
	Branch     string // the best verified integration tip (for a gated promote)
	Iterations int    // outer-loop iterations consumed (a termination witness)
	Promoted   bool   // a gated PromoteToBase was approved and applied
	Unmet      int    // criteria still unmet at termination (0 when Done)
	Summary    string // the loop's own account (data, never authoritative)
}

// Reasons — the closed set of terminal conditions. Each ceiling is INDEPENDENT and
// maps to a DISTINCT Reason so a caller (and the audit log) can tell exactly which
// rail fired.
const (
	ReasonConverged     = "converged"      // done-detection: the verifier verdict
	ReasonMaxIterations = "max_iterations" // the iteration ceiling
	ReasonNoProgress    = "no_progress"    // MaxNoProgress consecutive stalls → stop-ask
	ReasonBudget        = "budget"         // the budget Ledger ErrCeiling
	ReasonDeadline      = "deadline"       // the wall-clock Deadline / ctx
	ReasonLogBroken     = "log_broken"     // a degraded audit trail (I5 halt-gate)
)

const (
	defaultMaxIterations = 12
	defaultMaxNoProgress = 3
	projectTask          = "project" // log/budget key for the outer loop
)

// Run drives the loop from goal to a verifier-green tree, bounded by every ceiling
// at once. It returns the Outcome and an error only for an unrecoverable harness
// fault (a degraded audit trail). A failed verify, a Plan error, or a red slice
// are RESULTS the loop reflects on, never returned as errors — mirroring native.go,
// where a failing check is a result, not a fault.
//
// Termination is provable: Iteration increments by exactly one per pass and the
// loop's only continuation path falls through to `st.Iteration++; continue`. Every
// ceiling check returns before that, so the loop runs at most MaxIterations passes
// and each pass does a bounded amount of work. There is no path that loops without
// advancing the counter.
func (l *Loop) Run(ctx context.Context) (Outcome, error) {
	maxIter := l.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}

	st := State{Goal: l.Goal, Repo: l.Repo, Summary: summarize.ContextSummary{Goal: l.Goal}}
	st.VerifyCmd = verify.Detect(l.Repo)
	st.Criteria = append([]Criterion(nil), l.seeded...) // snapshot the seeded bar

	l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_start",
		Detail: map[string]any{"max_iterations": maxIter, "verify_cmd": st.VerifyCmd,
			"criteria": len(st.Criteria)}})

	noProgress := 0
	lastUnmet := -1 // sentinel: no prior measurement yet
	failRung := 0   // recovery-ladder rung for consecutive hard failures (plan/slice errors)

	for st.Iteration = 0; st.Iteration < maxIter; st.Iteration++ {
		// --- Halt-gates, polled at every iteration boundary (each a DISTINCT rail) ---

		// I5: an unverifiable history is worse than continuing. A broken trail halts.
		if err := l.Log.Err(); err != nil {
			return l.finish(st, false, ReasonLogBroken, 0), fmt.Errorf("project: audit trail degraded: %w", err)
		}
		// Wall-clock and cancellation: a hard rail independent of the model.
		if reason, stop := l.clockExpired(ctx); stop {
			return l.finish(st, false, reason, l.unmet(ctx, st)), nil
		}
		// Budget: a tiny reservation probe surfaces an exhausted global ceiling before
		// the loop spends a full model turn. ErrCeiling here means there is no headroom
		// left for even a negligible charge (the wall is already met).
		if l.budgetTripped(ctx) {
			return l.finish(st, false, ReasonBudget, l.unmet(ctx, st)), nil
		}

		// --- Done-detection: the verifier verdict, never an LLM claim (I2) ---
		if st.Iteration > 0 || len(st.Criteria) > 0 {
			done, unmet := l.judge(ctx, st)
			if done {
				return l.converge(ctx, st), nil
			}
			// Measure progress by the unmet-criteria count: strictly fewer unmet is
			// progress; equal-or-worse is a stall toward the no-progress ceiling.
			if lastUnmet >= 0 && unmet >= lastUnmet {
				noProgress++
			} else {
				noProgress = 0
			}
			lastUnmet = unmet
			l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_verify",
				Detail: map[string]any{"iteration": st.Iteration, "unmet": unmet, "no_progress": noProgress}})

			if l.noProgressCeiling(noProgress) {
				// The no-progress rail routes through the recovery ladder's final rung:
				// stop-and-ask the human. A "keep going" resets the stall counter; a
				// "stop" (or no channel) ends the run on the best verified tip.
				if l.askContinue(ctx, st, noProgress) {
					noProgress = 0
				} else {
					return l.finish(st, false, ReasonNoProgress, unmet), nil
				}
			}
		}

		// --- Plan the next slice (agentic; an error is a recoverable signal) ---
		sl, err := l.Plan(ctx, st.Goal, st)
		if err != nil {
			// A planning failure does not abort: the reflect ladder decides whether to
			// narrow, switch approach, or stop-and-ask. It climbs a rung per consecutive
			// hard failure (narrow → switch → stop) and never returns an error.
			act := l.reflect(ctx, st, fmt.Sprintf("planning failed: %v", err), failRung)
			failRung++
			if act == ladderStop {
				return l.finish(st, false, ReasonNoProgress, l.unmet(ctx, st)), nil
			}
			continue // narrow/switch: take another bounded pass
		}
		l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_slice_planned",
			Detail: map[string]any{"iteration": st.Iteration, "goal_len": len(sl.Goal)}})

		// --- Run the slice (supervisor spawn + integrator merge → one subtree) ---
		res, err := l.RunSlice(ctx, sl, st)
		if err != nil {
			// A budget ceiling surfacing through the supervisor is a stop signal, not a
			// crash — end on the last verified tip (design §7), mirroring super.Run.
			if errors.Is(err, budget.ErrCeiling) {
				return l.finish(st, false, ReasonBudget, l.unmet(ctx, st)), nil
			}
			act := l.reflect(ctx, st, fmt.Sprintf("slice failed: %v", err), failRung)
			failRung++
			if act == ladderStop {
				return l.finish(st, false, ReasonNoProgress, l.unmet(ctx, st)), nil
			}
			continue
		}

		// A clean slice resets the hard-failure ladder: progress on the bounded loop
		// returns the recovery rung to the bottom (narrow) for the next failure.
		failRung = 0

		// A verified slice advances the integration tip and folds bounded state
		// forward. A red slice keeps the PREVIOUS tip (the integrator guarantees the
		// merged subset is green) — already-merged verified work is never lost.
		if res.Verified && res.Branch != "" {
			st.Branch = res.Branch
		}
		if res.Summary.Goal != "" || len(res.Summary.Decisions) > 0 || res.Summary.Remaining != "" {
			st.Summary = res.Summary
		}
		l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_slice_done",
			Detail: map[string]any{"iteration": st.Iteration, "verified": res.Verified, "branch": st.Branch}})
	}

	// Iterations exhausted: a hard count rail. Hand back the best verified tip and
	// the unmet count so the caller can decide (re-plan more, or promote partial).
	return l.finish(st, false, ReasonMaxIterations, l.unmet(ctx, st)), nil
}

// converge handles the done path: the verifier (and every criterion) passed. The
// loop's job is to surface the green tip for a gated promote — it does NOT promote
// autonomously here; the single irreversible PromoteToBase is attempted only when a
// Gate seam is wired, via the structured action (reversible work never gates).
func (l *Loop) converge(ctx context.Context, st State) Outcome {
	out := l.finish(st, true, ReasonConverged, 0)
	out.Branch = st.Branch
	if l.Gate != nil && st.Branch != "" {
		// Cross-model review BEFORE the human gate (adaptive routing, P3-T04): a
		// strong reviewer model inspects the promote diff and can deny ahead of the
		// human approver. route.Review denies on any unparseable output (safe
		// default), so a model failure only ever blocks a promote, never waves one
		// through. Skipped entirely when no Reviewer/Differ is wired.
		if l.Reviewer != nil && l.Differ != nil {
			if diff, derr := l.Differ(st.Branch); derr == nil {
				approved, notes, rerr := route.Review(ctx, l.Reviewer, diff)
				l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_review",
					Detail: map[string]any{"branch": st.Branch, "approved": approved, "notes": notes}})
				if rerr != nil || !approved {
					return out // model review denied → never reach the human gate
				}
			}
		}
		action := policy.GateAction{Type: policy.PromoteToBase, Branch: st.Branch,
			Detail: "promote converged, verifier-green integration tip"}
		if l.Gate(action) {
			out.Promoted = true
			l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_promote",
				Detail: map[string]any{"branch": st.Branch, "approved": true}})
		} else {
			l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_promote",
				Detail: map[string]any{"branch": st.Branch, "approved": false}})
		}
	}
	_ = ctx
	return out
}

// finish builds the terminal Outcome and logs project_done. Done/Unmet are the
// verifier's accounting, never a model claim (I2).
func (l *Loop) finish(st State, done bool, reason string, unmet int) Outcome {
	l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_done",
		Detail: map[string]any{"done": done, "reason": reason, "iterations": st.Iteration, "unmet": unmet}})
	return Outcome{
		Done:       done,
		Reason:     reason,
		Branch:     st.Branch,
		Iterations: st.Iteration,
		Unmet:      unmet,
		Summary:    st.Summary.Remaining,
	}
}

// unmet counts the acceptance criteria still failing right now (a best-effort
// snapshot for the Outcome). It re-judges; on no criteria it returns 0.
func (l *Loop) unmet(ctx context.Context, st State) int {
	_, n := l.judge(ctx, st)
	return n
}
