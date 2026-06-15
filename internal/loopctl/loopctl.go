// Package loopctl is the shared cancel-cause discriminator for the streaming
// front door (ST-T01; first introduced as C1-T02, removed with CV-T01, and
// re-created here because the streaming interrupt needs it again). When a
// bounded model loop (native or supervisor) wraps a single iteration's model
// call — now a streaming Stream call — in a cancellable context, a returned
// cancellation can mean one of three very different things, and the loop must
// react differently to each:
//
//   - a SHUTDOWN/DEADLINE cancel — the task context died (SIGTERM, a parent
//     timeout, an enclosing context.WithDeadline). This is a hard termination
//     rail: the loop must unwind and return cleanly, NOT continue.
//   - a STEER cancel — the principal typed a steer mid-call, so the loop
//     deliberately cancelled its in-flight (possibly streaming) model call to
//     fold the steering text in at the next boundary. This is NOT an error: the
//     loop logs it and continues, keeping any partial text already streamed.
//   - a genuine FAULT — the model call failed for some other reason (a transport
//     error). This is the existing error path.
//
// Getting this discrimination wrong is the difference between overrunning a hard
// termination rail (a shutdown mistaken for a steer continues the loop past
// SIGTERM) and aborting a healthy run (a steer mistaken for a fatal fault kills
// the conversation). So the judgment lives HERE, in one tiny stdlib-only leaf
// that both loops import, rather than being re-implemented locally in each loop
// where the two copies could drift.
//
// The mechanism is the Go 1.21+ stdlib pair context.WithCancelCause +
// context.Cause: the per-iteration watcher cancels with ErrSteer as the cause,
// and ClassifyCancel reads that cause back out. No shared mutable "steered" bool
// exists anywhere — the cause travels inside the context — so the discriminator
// is race-free by construction (a hard acceptance criterion: `go test -race` is
// green) and stdlib-only (I6: the core has zero external dependencies).
package loopctl

import (
	"context"
	"errors"
)

// ErrSteer is the sentinel cancel cause a loop's steer watcher passes to its
// per-iteration context.CancelCauseFunc when the principal steers. It is the
// ONLY way a steer is distinguished from a shutdown/deadline cancel: the loop
// cancels the iteration context with ErrSteer as the cause, and ClassifyCancel
// recovers it via context.Cause. It is exported so both the native and the
// supervisor loops cancel with the SAME cause this package classifies — the
// single source of truth that keeps the two loops from drifting.
//
// Compare with errors.Is(context.Cause(iterCtx), loopctl.ErrSteer) rather than
// `==`: context.Cause wraps nothing here, but errors.Is is the canonical,
// future-proof sentinel check.
var ErrSteer = errors.New("steered")

// Kind is the discriminated cause of a loop iteration's cancellation.
type Kind int

const (
	// Shutdown means the TASK context was cancelled (SIGTERM, a parent
	// deadline, an enclosing cancel). It STRICTLY DOMINATES: if the task
	// context is done, the cancel is a shutdown even if a steer also fired,
	// because the bounded-loop / clean-shutdown rail must never be overrun by
	// a racing steer. The loop returns cleanly.
	Shutdown Kind = iota

	// Steer means the iteration context was cancelled with ErrSteer as its
	// cause while the task context is still live — the principal steered. The
	// loop logs it and continues; the steering text is folded in at the next
	// boundary. A steer is never an error.
	Steer

	// Fault means the cancellation was neither a shutdown nor a steer — a
	// genuine transport/model error. The loop takes its existing error path.
	Fault
)

func (k Kind) String() string {
	switch k {
	case Shutdown:
		return "shutdown"
	case Steer:
		return "steer"
	default:
		return "fault"
	}
}

// ClassifyCancel decides why a loop iteration's model call returned a
// cancellation error, given the long-lived TASK context and the per-iteration
// child context the call ran under.
//
// It evaluates in a FIXED precedence — and the order is load-bearing:
//
//  1. taskCtx.Err() != nil  ⇒ Shutdown. Checked FIRST so a shutdown ALWAYS wins
//     a shutdown-vs-steer race: a child context derived from a cancelled parent
//     is itself Done, so a steer that fires in the same instant as a SIGTERM can
//     never be misclassified as a steer and continue the loop past the
//     termination rail. Shutdown strictly dominates.
//  2. context.Cause(iterCtx) is ErrSteer ⇒ Steer. Only reached when the task
//     context is still live, so this is an isolated steer the loop should fold
//     in and continue past — never an error.
//  3. otherwise ⇒ Fault — a genuine transport/model error the loop surfaces on
//     its existing error path.
//
// It is a pure function over the two contexts' state: it reads no shared mutable
// flag, mutates nothing, and is safe to call from any goroutine — so it adds no
// data race regardless of how the iteration context was cancelled.
//
// Note: when neither context carries a cancellation cause, a model call that
// returned a non-context error still classifies as Fault (the default), which is
// exactly the existing behavior — ClassifyCancel only ever RECLASSIFIES a cancel
// as Shutdown or Steer; everything else stays a Fault for the caller's existing
// error path.
func ClassifyCancel(taskCtx, iterCtx context.Context) Kind {
	// (1) Shutdown strictly dominates: if the task context is done, this is a
	// termination, even if a steer also fired. Never continue past this.
	if taskCtx.Err() != nil {
		return Shutdown
	}
	// (2) Task context still live, so an ErrSteer cause is a real steer.
	if errors.Is(context.Cause(iterCtx), ErrSteer) {
		return Steer
	}
	// (3) Anything else is a genuine fault.
	return Fault
}
