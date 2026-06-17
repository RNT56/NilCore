package cron

import (
	"context"
	"testing"
	"time"

	"nilcore/internal/trigger"
)

func TestFiresOnInterval(t *testing.T) {
	var fired []trigger.Signal
	s := &Scheduler{
		Jobs: []Job{{Name: "nightly", Every: time.Hour, Goal: "bump deps", Source: "cron"}},
		Fire: func(_ context.Context, sig trigger.Signal) (bool, error) {
			fired = append(fired, sig)
			return true, nil
		},
	}
	t0 := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// First tick at start: not yet due (first fire is one interval out).
	if n := s.Tick(ctx, t0); n != 0 {
		t.Fatalf("tick at start fired %d, want 0", n)
	}
	// 30m later: still not due.
	if n := s.Tick(ctx, t0.Add(30*time.Minute)); n != 0 {
		t.Fatalf("tick at 30m fired %d, want 0", n)
	}
	// 1h later: due once.
	if n := s.Tick(ctx, t0.Add(time.Hour)); n != 1 {
		t.Fatalf("tick at 1h fired %d, want 1", n)
	}
	// 2h later: due again.
	if n := s.Tick(ctx, t0.Add(2*time.Hour)); n != 1 {
		t.Fatalf("tick at 2h fired %d, want 1", n)
	}
	if len(fired) != 2 {
		t.Fatalf("fired %d signals, want 2", len(fired))
	}
	if fired[0].Source != "cron" || fired[0].Goal != "bump deps" {
		t.Errorf("signal = %+v", fired[0])
	}
}

func TestZeroIntervalNeverFires(t *testing.T) {
	fired := 0
	s := &Scheduler{
		Jobs: []Job{{Name: "noop", Every: 0, Goal: "x"}},
		Fire: func(context.Context, trigger.Signal) (bool, error) { fired++; return true, nil },
	}
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		s.Tick(context.Background(), t0.Add(time.Duration(i)*time.Hour))
	}
	if fired != 0 {
		t.Fatalf("zero-interval job fired %d times", fired)
	}
}

func TestPollIntervalFloor(t *testing.T) {
	s := &Scheduler{Jobs: []Job{{Name: "fast", Every: 10 * time.Millisecond, Goal: "x"}}}
	if got := s.pollInterval(); got < time.Second {
		t.Errorf("poll interval = %v, want >= 1s floor", got)
	}
}

func TestWallClockDailyAt(t *testing.T) {
	var fired []trigger.Signal
	s := &Scheduler{
		Jobs: []Job{{Name: "report", At: "09:30", Goal: "post the daily report", Source: "cron"}},
		Fire: func(_ context.Context, sig trigger.Signal) (bool, error) {
			fired = append(fired, sig)
			return true, nil
		},
	}
	ctx := context.Background()
	loc := time.Local

	day := func(d, h, m int) time.Time { return time.Date(2026, 6, 16+d, h, m, 0, 0, loc) }

	// Start at 08:00 — seeds last; the 09:30 instant has not yet arrived today.
	if n := s.Tick(ctx, day(0, 8, 0)); n != 0 {
		t.Fatalf("start tick fired %d, want 0", n)
	}
	// 09:29 — still before the instant.
	if n := s.Tick(ctx, day(0, 9, 29)); n != 0 {
		t.Fatalf("09:29 fired %d, want 0", n)
	}
	// 09:31 — crossed 09:30, fire once.
	if n := s.Tick(ctx, day(0, 9, 31)); n != 1 {
		t.Fatalf("09:31 fired %d, want 1", n)
	}
	// 09:32 same day — already fired this instant, no double-fire.
	if n := s.Tick(ctx, day(0, 9, 32)); n != 0 {
		t.Fatalf("09:32 fired %d, want 0 (no double-fire)", n)
	}
	// 23:59 same day — next instant is tomorrow, still no fire.
	if n := s.Tick(ctx, day(0, 23, 59)); n != 0 {
		t.Fatalf("23:59 fired %d, want 0", n)
	}
	// Next day 09:31 — crossed tomorrow's 09:30, fire again.
	if n := s.Tick(ctx, day(1, 9, 31)); n != 1 {
		t.Fatalf("next-day 09:31 fired %d, want 1", n)
	}
	if len(fired) != 2 || fired[0].Goal != "post the daily report" {
		t.Fatalf("fired %+v, want 2 daily-report signals", fired)
	}
}

func TestWallClockHourly(t *testing.T) {
	fired := 0
	s := &Scheduler{
		Jobs: []Job{{Name: "h", At: "@hourly", Goal: "g"}},
		Fire: func(context.Context, trigger.Signal) (bool, error) { fired++; return true, nil },
	}
	ctx := context.Background()
	loc := time.Local
	at := func(h, m int) time.Time { return time.Date(2026, 6, 16, h, m, 0, 0, loc) }

	s.Tick(ctx, at(10, 15)) // seed; top of 10:00 already passed at seed → not due
	if fired != 0 {
		t.Fatalf("seed fired %d, want 0", fired)
	}
	s.Tick(ctx, at(10, 59)) // still inside the 10:00 hour
	if fired != 0 {
		t.Fatalf("10:59 fired %d, want 0", fired)
	}
	s.Tick(ctx, at(11, 1)) // crossed top of 11:00
	if fired != 1 {
		t.Fatalf("11:01 fired %d, want 1", fired)
	}
	s.Tick(ctx, at(11, 30)) // same hour, no double-fire
	if fired != 1 {
		t.Fatalf("11:30 fired %d, want 1", fired)
	}
	s.Tick(ctx, at(12, 0)) // crossed top of 12:00
	if fired != 2 {
		t.Fatalf("12:00 fired %d, want 2", fired)
	}
}

// A long downtime that skips several scheduled instants coalesces into ONE catch-up
// fire on the next tick — never a burst of stacked self-starts.
func TestWallClockCoalescesMissedInstants(t *testing.T) {
	fired := 0
	s := &Scheduler{
		Jobs: []Job{{Name: "d", At: "@daily", Goal: "g"}},
		Fire: func(context.Context, trigger.Signal) (bool, error) { fired++; return true, nil },
	}
	ctx := context.Background()
	loc := time.Local
	// Seed on day 0 at noon, then jump straight to day 5 (5 missed midnights).
	s.Tick(ctx, time.Date(2026, 6, 16, 12, 0, 0, 0, loc))
	if fired != 0 {
		t.Fatalf("seed fired %d, want 0", fired)
	}
	s.Tick(ctx, time.Date(2026, 6, 21, 12, 0, 0, 0, loc))
	if fired != 1 {
		t.Fatalf("after 5 missed midnights fired %d, want exactly 1 (coalesced)", fired)
	}
}

func TestValidAtAndParse(t *testing.T) {
	for _, c := range []struct {
		at   string
		want bool
	}{
		{"@hourly", true}, {"@daily", true},
		{"09:30", true}, {"9:5", true}, {"00:00", true}, {"23:59", true},
		{"24:00", false}, {"09:60", false}, {"-1:00", false},
		{"9", false}, {"abc", false}, {"09:30am", false}, {"", false}, {"@weekly", false},
	} {
		if got := ValidAt(c.at); got != c.want {
			t.Errorf("ValidAt(%q) = %v, want %v", c.at, got, c.want)
		}
	}
}

// A wall-clock job contributes a sub-minute poll so its instants fire punctually,
// without the 1s floor busy-tick an interval-less scheduler would otherwise pick.
func TestWallClockPollInterval(t *testing.T) {
	s := &Scheduler{Jobs: []Job{{Name: "d", At: "@daily", Goal: "g"}}}
	if got := s.pollInterval(); got != wallClockPoll {
		t.Errorf("poll interval = %v, want %v", got, wallClockPoll)
	}
}

// An unparseable At never fires and logs cron_badspec once (not per poll).
func TestBadAtNeverFires(t *testing.T) {
	fired := 0
	s := &Scheduler{
		Jobs: []Job{{Name: "bad", At: "99:99", Goal: "g"}},
		Fire: func(context.Context, trigger.Signal) (bool, error) { fired++; return true, nil },
	}
	ctx := context.Background()
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		s.Tick(ctx, t0.Add(time.Duration(i)*time.Hour))
	}
	if fired != 0 {
		t.Fatalf("bad-spec job fired %d, want 0", fired)
	}
}

func TestDefaultSourceIsCron(t *testing.T) {
	var got trigger.Signal
	s := &Scheduler{
		Jobs: []Job{{Name: "j", Every: time.Minute, Goal: "g"}}, // no Source
		Fire: func(_ context.Context, sig trigger.Signal) (bool, error) { got = sig; return true, nil },
	}
	t0 := time.Now()
	s.Tick(context.Background(), t0)
	s.Tick(context.Background(), t0.Add(time.Minute))
	if got.Source != "cron" {
		t.Errorf("default source = %q, want cron", got.Source)
	}
}
