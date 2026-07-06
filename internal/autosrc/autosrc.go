// Package autosrc is the autonomy daemon's pluggable event-source registry and one
// bounded priority queue (Phase 16, Pillar 7 / AUTO-T03). Today the agent's
// self-start funnels are SEPARATE machines: file-signals (`nilcore watch`), cron
// (internal/cron), inbound webhooks (internal/scmhook), durable wakes
// (internal/wake), and — later — a standing-objectives backlog each drive their own
// loop and their own routing. autosrc unifies them: every machine becomes a Source
// feeding ONE prioritized queue, and one Daemon drains that queue and hands each
// admitted goal to a single injected handler under a concurrency cap. That is the
// "one queue" the roadmap calls for, with nothing new about WHAT gets started — only
// how the many sources converge.
//
// # Why a pull (Next) Source, not a push (registration) Source
//
// Two shapes were possible (the roadmap leaves the choice to the implementer):
//
//   - PUSH: a Source is handed the queue and calls Enqueue itself.
//   - PULL: a Source exposes Next(ctx) and the daemon runs a tiny pump goroutine per
//     source that calls Next and enqueues the result.
//
// This package picks PULL. It keeps the queue's bound, sequence assignment, and
// close/drain semantics entirely inside the daemon — a Source never touches them, so
// a misbehaving source cannot corrupt the heap, forge a FIFO sequence, or keep
// enqueuing past shutdown. A Source becomes a trivial, independently-testable
// producer of one signal at a time, and existing sources (cron's Fire, scmhook's
// Handle, a directory poll) adapt to it by handing their next Signal back instead of
// invoking a routing callback. The daemon owns concurrency and lifetime; the Source
// owns only "what's next."
//
// # Invariants
//
//   - I2 (verifier sole authority): the daemon ROUTES a goal to the injected handler;
//     it never marks work done, never skips a verify, and the handler is where the
//     existing verified/gated drivegate path runs. autosrc folds NO learned signal.
//   - I3 (no ambient authority): autosrc holds no secrets, no policy, no envelope; it
//     passes a plain goal string onward. Nothing here reaches the model.
//   - I5 (append-only log): the optional *eventlog.Log is WRITE-of-metadata only via
//     Append; autosrc never mutates or reads-as-authority any history.
//   - I7 (untrusted input is data): a Source's Signal.Goal is attacker-influenceable
//     (a webhook title, a file's contents). autosrc treats it strictly as data —
//     queued, ordered by a structural Priority integer, and passed to the handler —
//     never interpreted as an instruction. Only Priority and the enqueue sequence are
//     templated by the queue.
//
// Everything is default-off and nil-safe: a Daemon with no sources drains nothing; a
// nil *eventlog.Log disables audit without affecting routing; an unset queue bound is
// unbounded (no rejection path). Stdlib (container/heap, context, sync) + the
// trigger/eventlog leaves only (I6); a deps_test guard forbids the orchestrator.
package autosrc

import (
	"context"

	"nilcore/internal/eventlog"
	"nilcore/internal/trigger"
)

// Source is one producer feeding the unified queue. Next blocks until it has the
// next signal to enqueue, the context is cancelled, or the source is permanently
// exhausted.
//
// Contract:
//   - (sig, true, nil)  — a real signal to enqueue.
//   - (_, false, nil)   — the source is DONE: no more signals ever. The daemon stops
//     pumping it (a one-shot or drained source closes cleanly this way).
//   - (_, false, err)   — a transient error. The daemon logs it and stops pumping
//     THIS source; it does not tear down the whole daemon (one bad source must not
//     silence the others). A context-cancellation error is treated as a clean stop.
//
// A Source MUST honor ctx in any blocking wait so the daemon can shut it down. A
// Source is consumed by exactly one pump goroutine, so it need not be safe for
// concurrent Next calls.
type Source interface {
	Next(ctx context.Context) (sig QueuedSignal, ok bool, err error)
}

// Handler routes one admitted signal onward — in production, the verified/gated
// drivegate path. It is an INJECTED dependency precisely so this leaf never imports
// the orchestrator (I6 dependency direction). It receives the plain trigger.Signal
// (Source + Goal); a returned error is logged and does not stop the daemon. The
// handler — not the daemon — owns gating: the daemon can never bypass a gate because
// it does no work itself, it only forwards the goal.
type Handler func(ctx context.Context, sig trigger.Signal) error

// audit appends one metadata-only event when a log is wired (I5). nil-safe.
func (d *Daemon) audit(kind string, detail map[string]any) {
	if d != nil && d.log != nil {
		d.log.Append(eventlog.Event{Kind: kind, Detail: detail})
	}
}
