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
	"strconv"
	"strings"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/trigger"
)

// Job is one scheduled goal. A job fires by EITHER a wall-clock spec (At, when set)
// or a fixed interval (Every). With Every the first fire happens one interval after
// the scheduler starts; with At the first fire lands on the next matching wall-clock
// instant (never immediately). Set exactly one of At / Every.
type Job struct {
	Name   string        // stable id, for the audit log and de-dup of last-fire
	Every  time.Duration // interval between fires (used when At == ""; must be > 0 to fire)
	At     string        // wall-clock schedule (local): "@hourly" | "@daily" | "HH:MM" (24h). Overrides Every.
	Goal   string        // the natural-language task to self-start
	Source string        // Signal.Source label (defaults to "cron")
}

// wallClockPoll is the poll cadence a wall-clock job contributes to pollInterval: a
// scheduled instant (minute resolution) fires within this window of becoming due.
// 30s keeps @hourly/@daily/HH:MM jobs punctual without busy-ticking every second
// for hours.
const wallClockPoll = 30 * time.Second

// ValidAt reports whether at is a recognized wall-clock spec ("@hourly", "@daily",
// or "HH:MM" 24-hour local). The CLI uses it to reject a bad spec at parse time
// rather than letting a job silently never fire.
func ValidAt(at string) bool {
	switch at {
	case "@hourly", "@daily":
		return true
	}
	_, _, ok := parseHHMM(at)
	return ok
}

// parseHHMM parses "HH:MM" (24-hour, optionally zero-padded) strictly: any non-numeric
// or out-of-range component returns ok=false, so a malformed spec never fires at a
// wrong time. Stdlib only (I6).
func parseHHMM(s string) (hh, mm int, ok bool) {
	hStr, mStr, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hStr)
	m, err2 := strconv.Atoi(mStr)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// prevOccurrence returns the most recent scheduled instant at or before now for the
// wall-clock spec at, and whether at parsed. Built from now's LOCAL calendar fields
// (so "09:30" means 09:30 in the scheduler's timezone, not a UTC offset of it). A job
// is due when its last fire predates this instant — so a tick that crosses a
// scheduled instant fires exactly once, and instants missed during downtime coalesce
// into a single catch-up fire (N stacked self-starts after an outage is never what
// you want).
func prevOccurrence(at string, now time.Time) (time.Time, bool) {
	loc := now.Location()
	switch at {
	case "@hourly":
		return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, loc), true
	case "@daily":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), true
	}
	hh, mm, ok := parseHHMM(at)
	if !ok {
		return time.Time{}, false
	}
	occ := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, loc)
	if occ.After(now) {
		occ = occ.AddDate(0, 0, -1) // today's instant hasn't arrived; the previous one was yesterday
	}
	return occ, true
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

	last   map[string]time.Time // name -> last fire (or start) time
	warned map[string]bool      // name -> a bad At spec was already logged (warn once, not per-poll)
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

// Tick fires every job that is due as of now (its wall-clock instant has been
// crossed, or its interval has elapsed, since its last fire). It returns the number
// of jobs fired. Exposed (rather than only Run) so the schedule logic is testable
// with a controlled clock and no real time.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) int {
	s.init(now)
	fired := 0
	for _, j := range s.Jobs {
		if !s.due(j, now) {
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
			// A failed scheduled run must leave an audit trail: an unattended operator
			// otherwise has no record that a cron-driven run errored. The boolean (auto-
			// started?) is not needed here; the error is the signal worth recording (I5).
			if _, ferr := s.Fire(ctx, trigger.Signal{Source: src, Goal: j.Goal}); ferr != nil && s.Log != nil {
				s.Log.Append(eventlog.Event{Kind: "cron_fire_error",
					Detail: map[string]any{"job": j.Name, "source": src, "error": ferr.Error()}})
			}
		}
		fired++
	}
	return fired
}

// due reports whether job j should fire as of now. A wall-clock job (At set) is due
// when the tick has crossed a scheduled instant since the job's last fire — init
// seeds last to the scheduler's start, so the first fire lands on the NEXT instant,
// never immediately. An interval job (Every > 0) is due once Every has elapsed. A job
// with neither, or with an unparseable At, never fires (the bad spec is logged once).
func (s *Scheduler) due(j Job, now time.Time) bool {
	if j.At != "" {
		occ, ok := prevOccurrence(j.At, now)
		if !ok {
			s.warnBadSpec(j)
			return false
		}
		return s.last[j.Name].Before(occ)
	}
	if j.Every > 0 {
		return now.Sub(s.last[j.Name]) >= j.Every
	}
	return false
}

// warnBadSpec logs an unparseable At spec once per job, so a misconfigured schedule
// surfaces in the audit trail instead of silently never firing.
func (s *Scheduler) warnBadSpec(j Job) {
	if s.warned == nil {
		s.warned = make(map[string]bool, 1)
	}
	if s.warned[j.Name] {
		return
	}
	s.warned[j.Name] = true
	if s.Log != nil {
		s.Log.Append(eventlog.Event{Kind: "cron_badspec", Detail: map[string]any{"job": j.Name, "at": j.At}})
	}
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

// pollInterval is the smallest poll a job needs, floored at one second so a
// misconfigured tiny interval cannot busy-loop. Interval jobs contribute their Every;
// wall-clock jobs contribute wallClockPoll (their instants are minute-resolution, so
// a sub-minute poll is enough to fire them punctually without busy-ticking).
func (s *Scheduler) pollInterval() time.Duration {
	min := time.Duration(0)
	for _, j := range s.Jobs {
		cand := j.Every
		if j.At != "" {
			cand = wallClockPoll
		}
		if cand > 0 && (min == 0 || cand < min) {
			min = cand
		}
	}
	if min < time.Second {
		min = time.Second
	}
	return min
}
