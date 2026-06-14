package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrencyCap submits many tiny tasks and asserts the pool never runs
// more than maxConcurrent at once while still running every task. Run with
// -race to surface any unsynchronized access to the counters.
func TestConcurrencyCap(t *testing.T) {
	const (
		n   = 50
		cap = 2
	)

	var inFlight atomic.Int64 // live count, maintained by the task bodies
	var peak atomic.Int64     // independent high-water mark, cross-checked below

	s := New(cap)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("task-%d", i)
		s.Submit(Task{ID: id, Run: func(ctx context.Context) error {
			cur := inFlight.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			if cur > cap {
				t.Errorf("observed %d in flight, cap is %d", cur, cap)
			}
			time.Sleep(2 * time.Millisecond) // tiny body, widens the overlap window
			inFlight.Add(-1)
			return nil
		}})
	}

	s.Start(context.Background())
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}

	if got := s.Ran(); got != n {
		t.Errorf("ran %d tasks, want %d", got, n)
	}
	if got := s.MaxInFlight(); got > cap {
		t.Errorf("scheduler MaxInFlight = %d, want <= %d", got, cap)
	}
	if got := peak.Load(); got > cap {
		t.Errorf("task-observed peak = %d, want <= %d", got, cap)
	}
	if inFlight.Load() != 0 {
		t.Errorf("in-flight count = %d after drain, want 0", inFlight.Load())
	}
}

// TestStartBeforeSubmit verifies tasks submitted after Start are still picked
// up, and that the FIFO queue drains cleanly regardless of submit timing.
func TestStartBeforeSubmit(t *testing.T) {
	s := New(3)
	s.Start(context.Background())

	var done atomic.Int64
	for i := 0; i < 10; i++ {
		s.Submit(Task{ID: fmt.Sprintf("t-%d", i), Run: func(ctx context.Context) error {
			done.Add(1)
			return nil
		}})
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if done.Load() != 10 {
		t.Errorf("ran %d, want 10", done.Load())
	}
}

// TestErrorsAreJoinedNotFatal confirms one task's error neither halts the pool
// nor is lost: every task runs and Wait returns the joined errors.
func TestErrorsAreJoinedNotFatal(t *testing.T) {
	sentinel := errors.New("boom")
	s := New(2)

	var ran atomic.Int64
	for i := 0; i < 6; i++ {
		i := i
		s.Submit(Task{ID: fmt.Sprintf("t-%d", i), Run: func(ctx context.Context) error {
			ran.Add(1)
			if i%2 == 0 {
				return sentinel
			}
			return nil
		}})
	}

	s.Start(context.Background())
	err := s.Wait()

	if ran.Load() != 6 {
		t.Errorf("ran %d tasks, want 6 (an error must not starve the pool)", ran.Load())
	}
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("Wait err = %v, want it to join the sentinel", err)
	}
}

// TestCancellationDrains ensures a cancelled context lets Wait return: queued
// tasks are skipped (Run not invoked) and the pool unwinds.
func TestCancellationDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before any worker starts

	s := New(2)
	var ran atomic.Int64
	for i := 0; i < 20; i++ {
		s.Submit(Task{ID: fmt.Sprintf("t-%d", i), Run: func(ctx context.Context) error {
			ran.Add(1)
			return nil
		}})
	}

	s.Start(ctx)
	if err := s.Wait(); err == nil {
		t.Fatal("expected cancellation errors from Wait")
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("ran %d task bodies under a cancelled ctx, want 0", got)
	}
	if s.Ran() != 0 {
		t.Errorf("Ran() = %d under cancellation, want 0", s.Ran())
	}
}

// TestMinConcurrencyClamped checks the floor: New(0) still runs serially rather
// than deadlocking with zero workers.
func TestMinConcurrencyClamped(t *testing.T) {
	s := New(0)
	var ran atomic.Int64
	for i := 0; i < 5; i++ {
		s.Submit(Task{ID: fmt.Sprintf("t-%d", i), Run: func(ctx context.Context) error {
			ran.Add(1)
			return nil
		}})
	}
	s.Start(context.Background())
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if ran.Load() != 5 {
		t.Errorf("ran %d, want 5", ran.Load())
	}
	if s.MaxInFlight() > 1 {
		t.Errorf("MaxInFlight = %d with clamp to 1, want <= 1", s.MaxInFlight())
	}
}

// TestNilRunIsSafe documents that a Task with a nil Run is accounted for but
// does no work, so a malformed submission cannot wedge the drain.
func TestNilRunIsSafe(t *testing.T) {
	s := New(2)
	s.Submit(Task{ID: "nil-run"})
	s.Start(context.Background())
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if s.Ran() != 1 {
		t.Errorf("Ran() = %d, want 1", s.Ran())
	}
}
