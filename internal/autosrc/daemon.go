package autosrc

import (
	"context"
	"errors"
	"sync"
	"time"

	"nilcore/internal/eventlog"
)

// defaultRetryBackoff is how long a pump waits before re-attempting an enqueue that hit a
// full queue. A full queue is transient back-pressure, so the pump HOLDS the already-
// consumed signal and retries rather than dropping it or dying; the wait keeps a saturated
// daemon from hot-spinning. Small so a freed slot is claimed promptly; tests override it.
const defaultRetryBackoff = 25 * time.Millisecond

// Config configures a Daemon. The zero value is a usable, default-off daemon: no
// sources (drains nothing), unbounded queue, single-flight handler dispatch, no
// audit log.
type Config struct {
	// QueueCap bounds the in-flight queue depth. <= 0 means unbounded — the
	// default-off posture (an operator who sets no bound gets no rejection path). A
	// positive cap makes Enqueue fail fast with ErrQueueFull as back-pressure.
	QueueCap int
	// Concurrency caps how many signals the daemon hands to the handler at once.
	// <= 0 is normalized to 1 (serial dispatch), never "unbounded" — an unbounded
	// fan-out into the handler would defeat the point of a bounded daemon.
	Concurrency int
	// Log records metadata-only audit events (I5). Optional; nil disables audit.
	Log *eventlog.Log
}

// Daemon drains the unified bounded priority queue and dispatches each signal to the
// injected Handler under a concurrency cap. It owns the queue lifetime, the per-
// source pump goroutines, and the worker semaphore. It does NO work itself and holds
// NO authority — it forwards a goal to the handler, which owns verification and
// gating (I2/I3). Construct with New; the zero value is not usable.
type Daemon struct {
	q            *boundedQueue
	handler      Handler
	conc         int
	log          *eventlog.Log
	retryBackoff time.Duration // pump wait between enqueue retries when the queue is full
}

// New builds a Daemon with the given handler and config. A nil handler is allowed
// (the daemon drains and drops, logging each drop) so a misconfigured wiring degrades
// to a visible no-op rather than a panic — the default-off / nil-safe posture.
func New(handler Handler, cfg Config) *Daemon {
	conc := cfg.Concurrency
	if conc < 1 {
		conc = 1
	}
	return &Daemon{
		q:            newBoundedQueue(cfg.QueueCap),
		handler:      handler,
		conc:         conc,
		log:          cfg.Log,
		retryBackoff: defaultRetryBackoff,
	}
}

// Run starts a pump goroutine per source, then drains the queue and dispatches each
// signal to the handler under the concurrency cap, until ctx is cancelled and the
// queue has drained. It is the daemon's production driver and blocks until shutdown
// is complete (every in-flight handler returned and every pump exited), so a caller
// can rely on no goroutine outliving Run.
//
// Shutdown order on ctx cancel: the pumps observe ctx and stop producing; the queue
// is closed so the drain loop, once the backlog empties, observes closed-and-empty
// and returns; Run waits for outstanding handler invocations to finish. Signals
// already queued at cancel time are still drained (graceful, not lossy) unless the
// handler itself honors the (cancelled) ctx and returns early.
func (d *Daemon) Run(ctx context.Context, sources ...Source) error {
	if d == nil || d.q == nil {
		return errors.New("autosrc: nil daemon")
	}
	d.audit("autosrc_start", map[string]any{"sources": len(sources), "concurrency": d.conc, "queue_cap": d.q.cap})

	// Pumps: one goroutine per source, each pulling Next and enqueueing. A pump
	// stops on a done/errored source or on queue close, so it never leaks past Run.
	var pumps sync.WaitGroup
	for i, src := range sources {
		if src == nil {
			continue
		}
		pumps.Add(1)
		go func(idx int, s Source) {
			defer pumps.Done()
			d.pump(ctx, idx, s)
		}(i, src)
	}

	// Close the queue once ctx is cancelled so the drain loop can reach its
	// closed-and-empty terminal. Bounded to Run's lifetime via the local done chan.
	closeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-closeDone:
		}
		d.q.close()
	}()
	defer close(closeDone)

	err := d.drain(ctx)

	// Drain is done (queue closed + empty, or ctx cancelled). Wait for the pumps to
	// notice and exit so no goroutine outlives Run.
	pumps.Wait()
	d.audit("autosrc_stop", map[string]any{})
	return err
}

// pump pulls signals from one source and enqueues them until the source is done,
// errors, the queue closes, or ctx is cancelled. A momentarily FULL queue does NOT
// stop the pump — enqueueSignal holds the signal and retries (back-pressure, not
// death), so a saturated daemon slows a source without killing it. One bad source
// stops only itself (its pump exits); the daemon and the other pumps continue.
func (d *Daemon) pump(ctx context.Context, idx int, s Source) {
	for {
		sig, ok, err := s.Next(ctx)
		if err != nil {
			// A context cancellation is a clean stop, not a fault.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			d.audit("autosrc_source_error", map[string]any{"source": idx, "err": err.Error()})
			return
		}
		if !ok {
			d.audit("autosrc_source_done", map[string]any{"source": idx})
			return
		}
		if !d.enqueueSignal(ctx, idx, sig) {
			return // queue closed or ctx cancelled while (re)enqueuing — clean stop
		}
	}
}

// enqueueSignal admits one already-consumed signal into the queue, absorbing transient
// back-pressure so a full queue never KILLS the pump. This is the fix for the source-death
// bug: previously the pump RETURNED on ErrQueueFull, which left the source's feeder blocked
// forever on its channel send (the pull side was gone) and silently lost the signal already
// pulled from the source. A full queue is pressure, not death: enqueueSignal records the
// stall once, then backs off and retries the SAME signal until a slot frees — so no consumed
// signal is dropped and the source resumes producing once the daemon catches up. It returns
// true when the signal is enqueued (the pump continues) and false when the pump should stop:
// the queue closed, or ctx was cancelled (shutdown — dropping a not-yet-queued signal is
// correct there, since the graceful-drain promise covers only already-QUEUED work).
func (d *Daemon) enqueueSignal(ctx context.Context, idx int, sig QueuedSignal) bool {
	stalled := false
	for {
		switch err := d.q.enqueue(sig); {
		case err == nil:
			d.audit("autosrc_enqueued", map[string]any{"source": idx, "priority": sig.Priority, "signal_source": sig.Signal.Source})
			return true
		case errors.Is(err, ErrQueueClosed):
			return false // shutting down — stop quietly
		case errors.Is(err, ErrQueueFull):
			// Back-pressure. Record it once per stall (not per retry, so a long stall
			// does not flood the log), then wait and retry the SAME signal. The signal
			// is held, never dropped; the pump stays alive.
			if !stalled {
				d.audit("autosrc_backpressure", map[string]any{"source": idx, "signal_source": sig.Signal.Source})
				stalled = true
			}
			if !d.backoff(ctx) {
				return false // ctx cancelled while backing off — stop (a shutdown drop)
			}
		default:
			// enqueue only returns Full/Closed today; any other rejection stops this
			// pump, surfaced for audit (defensive).
			d.audit("autosrc_enqueue_rejected", map[string]any{"source": idx, "reason": err.Error()})
			return false
		}
	}
}

// backoff waits one retry interval or until ctx is cancelled, returning true to retry and
// false if ctx ended while waiting. A non-positive interval is normalized to the default so
// a misconfigured daemon never hot-spins on a full queue.
func (d *Daemon) backoff(ctx context.Context) bool {
	wait := d.retryBackoff
	if wait <= 0 {
		wait = defaultRetryBackoff
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// drain dequeues signals and dispatches each to the handler under the concurrency
// cap, returning once the queue is closed and empty (clean) or ctx is cancelled. The
// cap is a buffered-channel semaphore: at most d.conc handler invocations run at
// once. drain waits for all dispatched handlers before returning, so Run does not
// return while a handler is still touching the world.
func (d *Daemon) drain(ctx context.Context) error {
	sem := make(chan struct{}, d.conc)
	var workers sync.WaitGroup
	defer workers.Wait() // never return while a handler is still in flight

	for {
		sig, ok, err := d.q.dequeue(ctx)
		if err != nil {
			return err // ctx cancelled
		}
		if !ok {
			return nil // queue closed and fully drained — clean shutdown
		}

		// Acquire a worker slot; honor cancellation while blocked so a cancel during
		// saturation does not stall shutdown.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}

		workers.Add(1)
		go func(qs QueuedSignal) {
			defer workers.Done()
			defer func() { <-sem }()
			d.dispatch(ctx, qs)
		}(sig)
	}
}

// dispatch hands one signal's underlying trigger.Signal to the handler and records
// the outcome. A nil handler is a visible drop (logged), never a panic. A handler
// error is logged and swallowed: one failed routing must not tear down the daemon —
// the handler itself owns retry/gate semantics.
func (d *Daemon) dispatch(ctx context.Context, qs QueuedSignal) {
	if d.handler == nil {
		d.audit("autosrc_dropped", map[string]any{"reason": "nil-handler", "signal_source": qs.Signal.Source})
		return
	}
	d.audit("autosrc_dispatch", map[string]any{"priority": qs.Priority, "signal_source": qs.Signal.Source})
	if err := d.handler(ctx, qs.Signal); err != nil {
		d.audit("autosrc_handle_error", map[string]any{"signal_source": qs.Signal.Source, "err": err.Error()})
	}
}

// Backlog reports the current queue depth (for metrics/tests).
func (d *Daemon) Backlog() int {
	if d == nil || d.q == nil {
		return 0
	}
	return d.q.len()
}
