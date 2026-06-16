// Package cron is a time-driven trigger source: at configured intervals it emits
// a trigger.Signal into the existing reversible-auto-start / irreversible-gate
// machinery (internal/trigger). It is the scheduled counterpart to `nilcore watch`
// (which polls a signal directory) and adds a new Signal SOURCE, not a new
// mechanism.
//
// It is deliberately NOT internal/scheduler (a time-agnostic bounded-concurrency
// worker pool) and NOT internal/loopctl (a cancel-cause discriminator). Pure
// stdlib (invariant I6: time only).
//
// Headless posture: a scheduled run has no human at a console, so any irreversible
// goal it produces deny-defaults inside trigger.Handle (a nil/deny approver) and
// simply does not start — by design, surfaced via the trigger_gated audit event.
package cron

import (
	"context"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/trigger"
)

// Job is one scheduled goal. Every is the interval between fires; the first fire
// happens one interval after the scheduler starts (or after Now at construction).
type Job struct {
	Name   string        // stable id, for the audit log and de-dup of last-fire
	Every  time.Duration // interval between fires (must be > 0 to ever fire)
	Goal   string        // the natural-language task to self-start
	Source string        // Signal.Source label (defaults to "cron")
}

// Scheduler fires due jobs on a tick. The zero value is not usable; set at least
// Jobs and Fire. Now defaults to time.Now.
type Scheduler struct {
	Jobs []Job
	// Fire routes a due job's Signal (typically trigger.Trigger.Handle). Required.
	Fire func(ctx context.Context, sig trigger.Signal) (bool, error)
	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
	// Log records metadata-only audit events (invariant I5). Optional.
	Log *eventlog.Log

	last map[string]time.Time // name -> last fire (or start) time
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// init seeds last-fire times so the first fire lands one full interval after the
// scheduler's start, not immediately on the first tick.
func (s *Scheduler) init(start time.Time) {
	if s.last == nil {
		s.last = make(map[string]time.Time, len(s.Jobs))
		for _, j := range s.Jobs {
			s.last[j.Name] = start
		}
	}
}

// Tick fires every job whose interval has elapsed since its last fire, as of now.
// It returns the number of jobs fired. Exposed (rather than only Run) so the
// schedule logic is testable with a controlled clock and no real time.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) int {
	s.init(now)
	fired := 0
	for _, j := range s.Jobs {
		if j.Every <= 0 {
			continue
		}
		if now.Sub(s.last[j.Name]) < j.Every {
			continue
		}
		s.last[j.Name] = now
		src := j.Source
		if src == "" {
			src = "cron"
		}
		if s.Log != nil {
			s.Log.Append(eventlog.Event{Kind: "cron_fire", Detail: map[string]any{"job": j.Name, "source": src}})
		}
		if s.Fire != nil {
			_, _ = s.Fire(ctx, trigger.Signal{Source: src, Goal: j.Goal})
		}
		fired++
	}
	return fired
}

// Run ticks on a wall-clock interval until ctx is cancelled. The poll interval is
// the smallest job interval (clamped to a sane floor), so a job fires within one
// poll of becoming due. It is the production driver; tests use Tick directly.
func (s *Scheduler) Run(ctx context.Context) error {
	start := s.now()
	s.init(start)

	poll := s.pollInterval()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.Tick(ctx, s.now())
		}
	}
}

// pollInterval is the smallest configured job interval, floored at one second so a
// misconfigured tiny interval cannot busy-loop.
func (s *Scheduler) pollInterval() time.Duration {
	min := time.Duration(0)
	for _, j := range s.Jobs {
		if j.Every > 0 && (min == 0 || j.Every < min) {
			min = j.Every
		}
	}
	if min < time.Second {
		min = time.Second
	}
	return min
}
