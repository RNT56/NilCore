package loopctl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestClassifyCancel_Precedence covers the three discrimination cases and — the
// load-bearing one — the shutdown-vs-steer race where shutdown must dominate.
// Each case builds the exact context shape a loop uses: a per-iteration child
// derived from the task context via WithCancelCause, just as native.go and
// super.go do.
func TestClassifyCancel_Precedence(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the (taskCtx, iterCtx) pair in the state the loop would
		// observe right after the model call returned, plus a cleanup func.
		setup func() (taskCtx, iterCtx context.Context, cleanup func())
		want  Kind
	}{
		{
			// A steer fired while the task context is still live: the iter ctx
			// was cancelled with ErrSteer. Classifies as Steer → loop continues.
			name: "steer while task live",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx := context.Background()
				iterCtx, cancel := context.WithCancelCause(taskCtx)
				cancel(ErrSteer)
				return taskCtx, iterCtx, func() {}
			},
			want: Steer,
		},
		{
			// The task context itself was cancelled (SIGTERM / parent cancel).
			// Classifies as Shutdown → loop returns cleanly.
			name: "task cancelled (shutdown)",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx, cancelTask := context.WithCancel(context.Background())
				iterCtx, cancelIter := context.WithCancelCause(taskCtx)
				cancelTask() // SIGTERM/deadline: task ctx dies, iter ctx dies with it.
				return taskCtx, iterCtx, func() { cancelIter(nil) }
			},
			want: Shutdown,
		},
		{
			// THE RACE: a shutdown AND a steer both fire. taskCtx.Err() is
			// checked first, so shutdown strictly dominates — the loop must NOT
			// continue past the termination rail. Cancelling the iter ctx with
			// ErrSteer after the task ctx is already dead must still yield
			// Shutdown.
			name: "shutdown vs steer race — shutdown wins",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx, cancelTask := context.WithCancel(context.Background())
				iterCtx, cancelIter := context.WithCancelCause(taskCtx)
				cancelTask()         // shutdown
				cancelIter(ErrSteer) // steer fires in the same instant
				return taskCtx, iterCtx, func() {}
			},
			want: Shutdown,
		},
		{
			// Steer fired first, THEN the task context died. Still Shutdown:
			// the precedence is by current state, not arrival order — once the
			// task ctx is done, it is a shutdown regardless of an earlier steer.
			name: "steer then shutdown — shutdown wins",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx, cancelTask := context.WithCancel(context.Background())
				iterCtx, cancelIter := context.WithCancelCause(taskCtx)
				cancelIter(ErrSteer)
				cancelTask()
				return taskCtx, iterCtx, func() {}
			},
			want: Shutdown,
		},
		{
			// No cancellation at all (the model call returned some non-context
			// transport error). Default → Fault: the loop's existing error path.
			name: "no cancel — fault",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx := context.Background()
				iterCtx, cancel := context.WithCancelCause(taskCtx)
				return taskCtx, iterCtx, func() { cancel(nil) }
			},
			want: Fault,
		},
		{
			// The iter ctx was cancelled with a NON-steer cause while the task
			// ctx is live (e.g. a deadline-on-the-iteration that is not a
			// steer). Not ErrSteer, task ctx live → Fault, not Steer. Guards
			// against a steer being inferred from any cancellation.
			name: "iter cancelled with non-steer cause — fault",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx := context.Background()
				iterCtx, cancel := context.WithCancelCause(taskCtx)
				cancel(errors.New("some other cause"))
				return taskCtx, iterCtx, func() {}
			},
			want: Fault,
		},
		{
			// cancel(nil) records context.Canceled as the cause, which is not
			// ErrSteer. With the task ctx live this is a Fault, never a Steer —
			// a plain cancel without the sentinel is not a steer.
			name: "iter cancelled with nil cause — fault",
			setup: func() (context.Context, context.Context, func()) {
				taskCtx := context.Background()
				iterCtx, cancel := context.WithCancelCause(taskCtx)
				cancel(nil)
				return taskCtx, iterCtx, func() {}
			},
			want: Fault,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			taskCtx, iterCtx, cleanup := tc.setup()
			defer cleanup()
			if got := ClassifyCancel(taskCtx, iterCtx); got != tc.want {
				t.Fatalf("ClassifyCancel = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParentCancelNotMisclassifiedAsSteer is the dedicated guard for the task's
// acceptance criterion: a parent (shutdown/deadline) cancel must NOT be reported
// as a steer. It uses WithDeadline (an already-elapsed deadline) so the task ctx
// is Done with DeadlineExceeded — the deadline form of shutdown — and asserts
// Shutdown even though the iter ctx is then cancelled with ErrSteer.
func TestParentCancelNotMisclassifiedAsSteer(t *testing.T) {
	// An already-past deadline: taskCtx is immediately Done(DeadlineExceeded).
	taskCtx, cancelTask := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancelTask()
	<-taskCtx.Done() // ensure the deadline has fired before we classify

	iterCtx, cancelIter := context.WithCancelCause(taskCtx)
	cancelIter(ErrSteer) // a steer that should be SHADOWED by the deadline
	defer cancelIter(nil)

	if !errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("precondition: taskCtx.Err() = %v, want DeadlineExceeded", taskCtx.Err())
	}
	if got := ClassifyCancel(taskCtx, iterCtx); got != Shutdown {
		t.Fatalf("deadline cancel classified as %v, want Shutdown (must not be Steer)", got)
	}
}

// TestSteerSentinelIsStable pins the sentinel's identity: ErrSteer compares with
// errors.Is to itself and not to context.Canceled, so the loop's cause check
// can't be accidentally satisfied by an ordinary cancel.
func TestSteerSentinelIsStable(t *testing.T) {
	if !errors.Is(ErrSteer, ErrSteer) {
		t.Fatal("ErrSteer is not errors.Is itself")
	}
	if errors.Is(ErrSteer, context.Canceled) {
		t.Fatal("ErrSteer must not match context.Canceled")
	}
	// Intentionally the reversed direction: this mirrors the loop's real check,
	// errors.Is(cause, ErrSteer) where cause is an ordinary context.Canceled, which
	// must stay false. SA1032's "wrong order" heuristic is a false positive here.
	//nolint:staticcheck // SA1032: reversed direction is the point of this assertion
	if errors.Is(context.Canceled, ErrSteer) {
		t.Fatal("context.Canceled must not match ErrSteer")
	}
}

// TestKindString covers the String() rendering used in log Details.
func TestKindString(t *testing.T) {
	for _, tc := range []struct {
		k    Kind
		want string
	}{
		{Shutdown, "shutdown"},
		{Steer, "steer"},
		{Fault, "fault"},
		{Kind(99), "fault"}, // unknown falls through to the fault label
	} {
		if got := tc.k.String(); got != tc.want {
			t.Fatalf("Kind(%d).String() = %q, want %q", tc.k, got, tc.want)
		}
	}
}

// TestClassifyCancel_RaceFree drives ClassifyCancel concurrently against a
// context pair whose cancellations are racing on real goroutines — the exact
// shutdown-vs-steer overlap the loop faces. Because ClassifyCancel reads no
// shared mutable state (the cause lives inside the context), this is a hard
// acceptance gate under `go test -race`: it must be clean. We do not assert a
// particular Kind (the race is nondeterministic) — only that classification
// produces a valid Kind and races nothing. We DO assert that once the task ctx
// is observed Done, the result is Shutdown (the dominance invariant) regardless
// of the racing steer.
func TestClassifyCancel_RaceFree(t *testing.T) {
	const iterations = 200
	var wg sync.WaitGroup

	for i := 0; i < iterations; i++ {
		taskCtx, cancelTask := context.WithCancel(context.Background())
		iterCtx, cancelIter := context.WithCancelCause(taskCtx)

		wg.Add(3)
		// Racer A: shutdown.
		go func() { defer wg.Done(); cancelTask() }()
		// Racer B: steer.
		go func() { defer wg.Done(); cancelIter(ErrSteer) }()
		// Racer C: the loop classifying mid-race.
		go func() {
			defer wg.Done()
			k := ClassifyCancel(taskCtx, iterCtx)
			// Re-check under the dominance invariant: if the task ctx is done at
			// the moment we re-read it, a steer must never have been the verdict
			// at a point where the task was already dead.
			if taskCtx.Err() != nil && ClassifyCancel(taskCtx, iterCtx) != Shutdown {
				t.Errorf("task done but classify != Shutdown (got %v)", k)
			}
		}()
		wg.Wait()
		cancelIter(nil)
	}
}
