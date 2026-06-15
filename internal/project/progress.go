package project

// progress.go owns the termination rails. The loop has MULTIPLE INDEPENDENT
// ceilings, and each maps to a DISTINCT Outcome.Reason so a caller (and the audit
// trail) can tell exactly which rail fired:
//
//	MaxIterations  → ReasonMaxIterations  (the iteration counter; the primary rail)
//	MaxNoProgress  → ReasonNoProgress     (consecutive stalls → stop-and-ask)
//	budget ceiling → ReasonBudget         (the shared Ledger ErrCeiling)
//	Deadline / ctx → ReasonDeadline        (the wall clock)
//	done-detection → ReasonConverged      (the verifier verdict, judge.go)
//
// The rails are mutually independent: removing any one still leaves the loop
// bounded (MaxIterations alone caps the pass count; the rest only end it sooner).
// That redundancy is deliberate — the budget rail is soft (design risk #1: it does
// nothing until the meter is wired), so termination must NOT depend on it, and it
// does not.

import (
	"context"
	"errors"
	"time"

	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/guard"
)

// budgetReservation is the negligible dollar amount the loop reserves to probe the
// ledger for remaining headroom. It is far below any real sub-cent model cost, so
// the recorded noise across a whole run is immaterial, yet it is strictly above the
// ledger's rounding epsilon so a charge AT the ceiling is refused (not absorbed).
// This mirrors the meter's §7 "pre-charge a small reservation" shape.
const budgetReservation = 1e-6

// budgetTripped reports whether the shared ledger's global (or this task's) ceiling
// has no headroom left. It probes with a tiny reservation charge: Charge refuses
// any charge that would push spend OVER the ceiling, so when the wall is already
// met even this negligible charge is refused and returns ErrCeiling. A nil ledger
// never trips (the budget rail is soft, so the loop must stay bounded without it —
// design §1). The real spend wall is the meter charging full model turns inside
// Plan/RunSlice, which propagates ErrCeiling up; this probe just stops a doomed
// iteration one model turn earlier.
func (l *Loop) budgetTripped(ctx context.Context) bool {
	if l.Budget == nil {
		return false
	}
	err := l.Budget.Charge(ctx, projectTask, 0, budgetReservation)
	return errors.Is(err, budget.ErrCeiling)
}

// clockExpired reports whether the wall-clock Deadline or ctx has elapsed, and the
// DISTINCT Reason for it. A cancelled/expired ctx and a passed Deadline both map to
// the single wall-clock rail (ReasonDeadline); the caller halts on stop==true. A
// zero Deadline disables the wall-clock check (ctx cancellation still applies).
func (l *Loop) clockExpired(ctx context.Context) (reason string, stop bool) {
	if err := ctx.Err(); err != nil {
		return ReasonDeadline, true
	}
	if !l.Deadline.IsZero() && !time.Now().Before(l.Deadline) {
		return ReasonDeadline, true
	}
	return "", false
}

// noProgressCeiling reports whether consecutive stalls have reached the
// MaxNoProgress rail. A non-positive MaxNoProgress uses the default, so the rail is
// ALWAYS finite — a stalling run can never spin all the way to MaxIterations
// without first hitting the (cheaper) no-progress stop-ask.
func (l *Loop) noProgressCeiling(noProgress int) bool {
	cap := l.MaxNoProgress
	if cap <= 0 {
		cap = defaultMaxNoProgress
	}
	return noProgress >= cap
}

// askContinue is the no-progress rail's bridge into the recovery ladder's final
// rung: stop-and-ask the human (I3 — no ambient authority; the human decides
// whether to keep spending). It returns true only on an explicit human "keep
// going"; a nil Channel, an ask error, or a human "stop" all return false, so the
// safe default when no human is reachable is to STOP rather than burn the remaining
// iteration budget unsupervised.
//
// The question carries only bounded, fenced state (the goal and the stall count) —
// never a transcript. The human's answer is a control decision, not data the model
// consumes, so it is not guard-fenced; the goal we echo back to the human IS fenced
// as data so a goal string can never smuggle an instruction into the prompt (I7).
func (l *Loop) askContinue(ctx context.Context, st State, noProgress int) bool {
	l.Log.Append(eventlog.Event{Task: projectTask, Kind: "project_no_progress",
		Detail: map[string]any{"iteration": st.Iteration, "no_progress": noProgress}})
	if l.Channel == nil {
		return false // no human reachable → stop on the best verified tip
	}
	q := "No progress for " + itoa(noProgress) + " iterations on:\n" +
		guard.Wrap("project goal", st.Goal) +
		"\nKeep going (more iterations) or stop here with the best verified tree?"
	ok, err := l.Channel.Ask(ctx, projectTask, q)
	if err != nil {
		return false // an unreachable human is a stop, never a silent "keep going"
	}
	return ok
}

// itoa is a tiny dependency-free int→string for the prompt (stdlib strconv would
// do, but keeping the one call inline avoids an import for a single use).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
