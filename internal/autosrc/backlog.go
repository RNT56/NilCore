package autosrc

// backlog.go — the standing-objectives backlog Source (Phase 16, Pillar 7 / AUTO-T05).
//
// This is the daemon's IDLE self-service funnel: when the agent has no foreground
// work it pulls the highest-priority *due* standing objective from an
// objective.Backlog and feeds it into the same unified queue as every reactive
// source — only at a LOWER priority band, so reactive work (a failing CI signal, a
// webhook, a cron fire) always preempts idle backlog work. The source only PRODUCES
// a goal; the daemon's injected handler still routes it through the verified/gated
// drivegate path. This source never runs, verifies, or gates the work itself (I2):
// it is a pull producer of "the next idle goal," nothing more.
//
// # Why a poll Source with an idle gate
//
// A standing objective is "due" by a clock (its MinPeriod since LastRun), not by an
// external event — there is no channel to block on. So this Source POLLS: each Next
// asks the backlog whether anything is due *and* whether the daemon is idle, and if
// not it waits one Interval (honoring ctx) before asking again. The idle predicate is
// injected so the source never reaches into the orchestrator to learn busy-ness — the
// wiring layer supplies "am I idle?" exactly as it supplies the handler (I6 direction:
// the daemon is installed by the orchestrator, never the reverse).
//
// MarkAttempt is called the moment a due objective is SELECTED (debounced by the
// objective leaf's own LastRun cadence), so the same objective is not re-emitted on the
// very next poll before its run has a chance to start. It advances only the DEBOUNCE
// clock, NOT a completion claim: a selected run that later fails or is gate-denied
// re-arms after the objective's (shorter) RetryPeriod rather than a full MinPeriod, so a
// standing objective is not silently parked for a whole period after an unverified run.
// The handler signals a verified outcome separately via the backlog's MarkSuccess (the
// daemon wiring) — this source only selects + debounces, it never marks work done (I2).
// The advance timestamp is the same injected `now` used for due-selection, so selection
// and debounce share one clock — deterministic, never wall-clock (tests inject both
// `now` and the poll wait).
//
// # Invariants
//
//   - I2: the source emits a goal; it never marks work done or skips a verify.
//     MarkAttempt advances only the backlog's debounce clock — it is NOT a completion
//     claim (the verified outcome is recorded separately by the handler's MarkSuccess).
//   - I3: holds no secret/policy/envelope; emits a plain Goal string. Nothing reaches
//     the model.
//   - I7: Objective.Goal is operator-authored text treated strictly as data — queued,
//     ordered by the structural Priority integer, passed to the handler, never
//     interpreted as an instruction. Only the structural Priority is templated.
//
// Default-off: with no objectives in the store NextIdle yields nothing, so Next simply
// polls forever and emits nothing — the backlog source stays inert until an operator
// adds a standing objective. A nil idle predicate means "always idle" (a daemon with
// no foreground concept), so the source degrades to a pure due-poller.

import (
	"context"
	"time"

	"nilcore/internal/objective"
	"nilcore/internal/trigger"
)

// DefaultBacklogPriority is the priority band the backlog source emits at unless an
// operator overrides it. It is deliberately the LOWEST band — BELOW PriorityFile (the
// 0 reactive floor in adapters.go) — so EVERY reactive source (file/cron/wake/webhook)
// preempts idle backlog work in the shared queue. (An operator wanting backlog above a
// given reactive source can still set BacklogConfig.Priority to a positive band.)
const DefaultBacklogPriority = -1

// DefaultBacklogInterval is the poll spacing the backlog source waits between "is
// anything due / am I idle?" checks when it has nothing to emit. It bounds how often
// the source touches the backlog store so an idle daemon does not hot-spin.
const DefaultBacklogInterval = 30 * time.Second

// objectiveBacklog is the narrow slice of *objective.Backlog this source needs: pick
// the next due objective and advance its debounce clock. Declaring it as an interface
// keeps the source unit-testable against an in-memory fake without standing up a Store,
// and documents that the source only READS-to-select and advances the debounce — it
// never edits, re-prioritizes, or deletes an objective (operator-only, per the
// objective package doc).
type objectiveBacklog interface {
	NextIdle(ctx context.Context, now time.Time) (objective.Objective, bool, error)
	MarkAttempt(ctx context.Context, id string, when time.Time) error
}

// compile-time proof the concrete backlog satisfies the seam.
var _ objectiveBacklog = (*objective.Backlog)(nil)

// BacklogSource is an autosrc.Source backed by an objective.Backlog. When the daemon
// is idle it emits the highest-priority due standing objective as a QueuedSignal at a
// low priority band (so reactive work preempts it), then marks it run. Construct with
// NewBacklogSource; the zero value is not usable.
type BacklogSource struct {
	backlog  objectiveBacklog
	priority int
	interval time.Duration

	// idle reports whether the daemon currently has no foreground work; only then does
	// the source pull. nil ⇒ always idle (pure due-poller).
	idle func() bool
	// now supplies the selection/debounce clock; nil ⇒ time.Now. Injected for tests.
	now func() time.Time
	// wait sleeps d honoring ctx; nil ⇒ a real ctx-aware timer. Injected for tests so
	// the poll loop is deterministic and never sleeps wall-clock.
	wait func(ctx context.Context, d time.Duration) error
}

// BacklogConfig configures a BacklogSource. The zero value is filled with safe
// defaults by NewBacklogSource: DefaultBacklogPriority, DefaultBacklogInterval, an
// always-idle predicate, and the wall clock.
type BacklogConfig struct {
	// Priority is the band emitted signals carry. <= 0 ⇒ DefaultBacklogPriority. Keep
	// it strictly below every reactive source so backlog work never preempts them.
	Priority int
	// Interval is the poll spacing when nothing is emitted. <= 0 ⇒ DefaultBacklogInterval.
	Interval time.Duration
	// Idle reports whether the daemon is idle (no foreground work). nil ⇒ always idle.
	Idle func() bool
	// Now supplies the deterministic clock for selection + debounce. nil ⇒ time.Now.
	Now func() time.Time
	// Wait sleeps for d, returning early (with ctx.Err()) if ctx is cancelled. nil ⇒ a
	// real ctx-aware timer. Tests inject this to drive the poll loop without real time.
	Wait func(ctx context.Context, d time.Duration) error
}

// NewBacklogSource builds a BacklogSource over an objective.Backlog (or any seam that
// satisfies objectiveBacklog, for tests). A nil backlog makes the source inert: Next
// just polls and never emits, so an unwired backlog source is byte-identically a
// no-op (the default-off contract).
func NewBacklogSource(backlog objectiveBacklog, cfg BacklogConfig) *BacklogSource {
	prio := cfg.Priority
	if prio <= 0 {
		prio = DefaultBacklogPriority
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultBacklogInterval
	}
	return &BacklogSource{
		backlog:  backlog,
		priority: prio,
		interval: interval,
		idle:     cfg.Idle,
		now:      cfg.Now,
		wait:     cfg.Wait,
	}
}

// Next implements Source. It loops: when the daemon is idle it asks the backlog for the
// next due objective; if one is due it marks it run and returns it as a QueuedSignal;
// otherwise (busy, or nothing due) it waits one interval and tries again. It blocks
// until it has a signal or ctx is cancelled, honoring the Source contract:
//
//   - (sig, true, nil)  — a due objective to enqueue.
//   - (_, false, err)   — ctx cancelled (a clean stop; the daemon treats it as such) or
//     a transient backlog error (the daemon stops THIS pump, not the whole daemon).
//
// It never returns (_, false, nil): a backlog source is not one-shot — it stays a live
// idle funnel for the daemon's lifetime, exhausted only by ctx cancellation.
func (s *BacklogSource) Next(ctx context.Context) (QueuedSignal, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return QueuedSignal{}, false, err
		}

		if s.isIdle() {
			now := s.clock()
			obj, ok, err := s.pull(ctx, now)
			if err != nil {
				return QueuedSignal{}, false, err
			}
			if ok {
				return QueuedSignal{
					// Source labels the funnel; Goal is operator-authored data passed
					// through untouched (I7). Priority is structural and LOW so reactive
					// sources preempt this idle work.
					Signal:   trigger.Signal{Source: "backlog", Goal: obj.Goal},
					Priority: s.priority,
				}, true, nil
			}
		}

		// Busy, or nothing due: wait one interval and re-poll. The wait honors ctx so a
		// shutdown unblocks the source promptly.
		if err := s.sleep(ctx, s.interval); err != nil {
			return QueuedSignal{}, false, err
		}
	}
}

// pull selects the next due objective at `now` and, if one is found, advances its
// debounce clock (MarkAttempt) before returning it. MarkAttempt is at selection time so
// the same objective is not re-emitted before its run starts; the advance uses the same
// `now` as selection so the two share one clock. It advances only the debounce — NOT a
// completion claim — so a run that later fails re-arms after the objective's RetryPeriod
// (the verified outcome is recorded by the handler via MarkSuccess, not here). A nil
// backlog is inert (nothing due).
func (s *BacklogSource) pull(ctx context.Context, now time.Time) (objective.Objective, bool, error) {
	if s.backlog == nil {
		return objective.Objective{}, false, nil
	}
	obj, ok, err := s.backlog.NextIdle(ctx, now)
	if err != nil {
		return objective.Objective{}, false, err
	}
	if !ok {
		return objective.Objective{}, false, nil
	}
	if err := s.backlog.MarkAttempt(ctx, obj.ID, now); err != nil {
		return objective.Objective{}, false, err
	}
	return obj, true, nil
}

func (s *BacklogSource) isIdle() bool {
	if s.idle == nil {
		return true
	}
	return s.idle()
}

func (s *BacklogSource) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// sleep waits d honoring ctx. The injected wait (tests) takes precedence; otherwise a
// real ctx-aware timer is used so a cancelled context unblocks the poll immediately.
func (s *BacklogSource) sleep(ctx context.Context, d time.Duration) error {
	if s.wait != nil {
		return s.wait(ctx, d)
	}
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
