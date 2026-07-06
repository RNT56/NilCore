package autosrc

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/trigger"
)

// sig is a tiny helper to build a QueuedSignal with a goal label and priority.
func sig(goal string, prio int) QueuedSignal {
	return QueuedSignal{Signal: trigger.Signal{Source: "test", Goal: goal}, Priority: prio}
}

// TestQueueOrdering proves the heap drains by Priority (descending), and within an
// equal priority band by enqueue order (stable FIFO via the assigned sequence).
func TestQueueOrdering(t *testing.T) {
	q := newBoundedQueue(0)

	// Interleave priorities and enqueue order. Equal-priority goals (the "b#" set at
	// priority 5) must come out in enqueue order; higher priority always precedes.
	in := []QueuedSignal{
		sig("b1", 5),
		sig("low", 1),
		sig("hi", 10),
		sig("b2", 5),
		sig("b3", 5),
		sig("mid", 3),
	}
	for _, s := range in {
		if err := q.enqueue(s); err != nil {
			t.Fatalf("enqueue %q: %v", s.Signal.Goal, err)
		}
	}

	var got []string
	for q.len() > 0 {
		s, ok, err := q.dequeue(context.Background())
		if err != nil || !ok {
			t.Fatalf("dequeue: ok=%v err=%v", ok, err)
		}
		got = append(got, s.Signal.Goal)
	}

	want := []string{"hi", "b1", "b2", "b3", "mid", "low"}
	if len(got) != len(want) {
		t.Fatalf("drained %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch at %d: got %v, want %v", i, got, want)
		}
	}
}

// TestBoundedEnqueueRejects proves a positive cap is a hard fence: the (cap+1)th
// enqueue returns ErrQueueFull, and an unbounded queue (cap<=0) never rejects.
func TestBoundedEnqueueRejects(t *testing.T) {
	q := newBoundedQueue(2)
	if err := q.enqueue(sig("a", 1)); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := q.enqueue(sig("b", 1)); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	err := q.enqueue(sig("c", 1))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("over-cap enqueue: got %v, want ErrQueueFull", err)
	}
	if q.len() != 2 {
		t.Fatalf("len after rejection = %d, want 2", q.len())
	}

	// Unbounded: many enqueues, none rejected.
	u := newBoundedQueue(0)
	for i := 0; i < 1000; i++ {
		if err := u.enqueue(sig("x", 0)); err != nil {
			t.Fatalf("unbounded enqueue %d rejected: %v", i, err)
		}
	}
}

// TestEnqueueAfterCloseRejects proves a closed queue refuses new work.
func TestEnqueueAfterCloseRejects(t *testing.T) {
	q := newBoundedQueue(0)
	q.close()
	if err := q.enqueue(sig("a", 1)); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("enqueue after close: got %v, want ErrQueueClosed", err)
	}
}

// TestDequeueHonorsCancel proves a dequeue blocked on an empty queue unparks and
// returns the context error when the context is cancelled (no enqueue happens).
func TestDequeueHonorsCancel(t *testing.T) {
	q := newBoundedQueue(0)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, ok, err := q.dequeue(ctx)
		if ok {
			done <- errors.New("unexpected item from empty cancelled queue")
			return
		}
		done <- err
	}()

	// Give the goroutine a moment to park in cond.Wait, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dequeue after cancel: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dequeue did not unpark on cancel")
	}
}

// TestDequeueClosedAndEmpty proves the clean drain-complete terminal: a closed,
// empty queue returns (zero, false, nil) — not an error.
func TestDequeueClosedAndEmpty(t *testing.T) {
	q := newBoundedQueue(0)
	q.close()
	_, ok, err := q.dequeue(context.Background())
	if ok || err != nil {
		t.Fatalf("closed-empty dequeue: ok=%v err=%v, want false,nil", ok, err)
	}
}

// errSource is a Source that always returns a transient error — the daemon must stop
// pumping it without tearing down the other sources.
type errSource struct{}

func (errSource) Next(context.Context) (QueuedSignal, bool, error) {
	return QueuedSignal{}, false, errors.New("boom")
}

// chanSource is a Source backed by a channel: it yields each buffered signal, then
// reports done (false,nil) when the channel closes. It honors ctx.
type chanSource struct{ ch chan QueuedSignal }

func (c *chanSource) Next(ctx context.Context) (QueuedSignal, bool, error) {
	select {
	case s, open := <-c.ch:
		if !open {
			return QueuedSignal{}, false, nil // exhausted
		}
		return s, true, nil
	case <-ctx.Done():
		return QueuedSignal{}, false, ctx.Err()
	}
}

// TestDaemonSourceReachesHandler proves a registered source's signals are drained and
// delivered to the injected handler, and that the daemon shuts down cleanly once the
// source is exhausted and the context is cancelled.
func TestDaemonSourceReachesHandler(t *testing.T) {
	var mu sync.Mutex
	var got []string
	handler := func(ctx context.Context, s trigger.Signal) error {
		mu.Lock()
		got = append(got, s.Goal)
		mu.Unlock()
		return nil
	}

	d := New(handler, Config{Concurrency: 2})

	ch := make(chan QueuedSignal, 3)
	ch <- sig("g1", 1)
	ch <- sig("g2", 1)
	ch <- sig("g3", 1)
	close(ch) // source becomes exhausted after these three

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx, &chanSource{ch: ch}) }()

	// Wait until all three are delivered, then cancel to end Run.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d/3 signals delivered", n)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	sort.Strings(got)
	want := []string{"g1", "g2", "g3"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("handler saw %v, want %v", got, want)
	}
}

// TestDaemonConcurrencyCap proves the daemon never runs more than Concurrency
// handler invocations at once.
func TestDaemonConcurrencyCap(t *testing.T) {
	const cap = 3
	var inFlight, maxSeen int32

	release := make(chan struct{})
	handler := func(ctx context.Context, s trigger.Signal) error {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
				break
			}
		}
		<-release // hold the slot until the test releases everyone
		atomic.AddInt32(&inFlight, -1)
		return nil
	}

	d := New(handler, Config{Concurrency: cap})

	ch := make(chan QueuedSignal, 20)
	for i := 0; i < 20; i++ {
		ch <- sig("g", 1)
	}
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx, &chanSource{ch: ch}) }()

	// Let the daemon saturate to the cap, then release.
	time.Sleep(100 * time.Millisecond)
	if m := atomic.LoadInt32(&maxSeen); m > cap {
		close(release)
		t.Fatalf("max in-flight %d exceeded concurrency cap %d", m, cap)
	}
	close(release)
	cancel()

	select {
	case <-runErr:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	if m := atomic.LoadInt32(&maxSeen); m > cap {
		t.Fatalf("max in-flight %d exceeded concurrency cap %d", m, cap)
	}
}

// TestNilHandlerDropsCleanly proves a nil handler is a visible no-op, never a panic
// (default-off / nil-safe posture).
func TestNilHandlerDropsCleanly(t *testing.T) {
	d := New(nil, Config{})
	ch := make(chan QueuedSignal, 1)
	ch <- sig("g", 1)
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx, &chanSource{ch: ch}) }()

	// Give it time to drain the one signal, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run with nil handler returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run with nil handler did not return")
	}
}

// TestDaemonNoSourcesDrainsNothing proves the default-off posture: a daemon with no
// sources returns promptly on cancel and delivers nothing.
func TestDaemonNoSourcesDrainsNothing(t *testing.T) {
	called := false
	d := New(func(ctx context.Context, s trigger.Signal) error { called = true; return nil }, Config{})

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-runErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with no sources did not return on cancel")
	}
	if called {
		t.Fatal("handler called despite no sources")
	}
}

// TestOneBadSourceDoesNotStopOthers proves a source that errors stops only its own
// pump; a healthy source's signal still reaches the handler.
func TestOneBadSourceDoesNotStopOthers(t *testing.T) {
	var mu sync.Mutex
	var got []string
	handler := func(ctx context.Context, s trigger.Signal) error {
		mu.Lock()
		got = append(got, s.Goal)
		mu.Unlock()
		return nil
	}
	d := New(handler, Config{})

	bad := errSource{}
	goodCh := make(chan QueuedSignal, 1)
	goodCh <- sig("survivor", 1)
	close(goodCh)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx, bad, &chanSource{ch: goodCh}) }()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("healthy source's signal never reached the handler")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	<-runErr

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "survivor" {
		t.Fatalf("handler saw %v, want [survivor]", got)
	}
}

// TestBacklogOnNilDaemon proves the nil-safe read surface.
func TestBacklogOnNilDaemon(t *testing.T) {
	var d *Daemon
	if d.Backlog() != 0 {
		t.Fatal("nil-daemon Backlog should be 0")
	}
}
