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
