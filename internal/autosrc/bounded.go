package autosrc

import (
	"container/heap"
	"context"
	"errors"
	"sync"
)

// ErrQueueFull is returned by enqueue when the bounded queue is at capacity. It is
// a value, not a failure of the daemon: a full queue means back-pressure (the
// daemon is saturated), which a source handles by retrying later or dropping —
// never by growing memory without bound. Detect it with errors.Is.
var ErrQueueFull = errors.New("autosrc: priority queue full")

// ErrQueueClosed is returned by enqueue (and surfaces from a blocked dequeue) once
// the queue is closed during shutdown. Detect it with errors.Is.
var ErrQueueClosed = errors.New("autosrc: priority queue closed")

// boundedQueue is the daemon's single ingress: a capacity-bounded max-priority
// queue (queue.go's pq) made concurrency-safe and blocking. Many source goroutines
// enqueue; the daemon's drain loop dequeues. A sync.Cond wakes a waiting dequeue
// the instant an enqueue lands or the queue closes, so the drain loop never busy-
// polls. The bound is a hard memory fence: enqueue past cap fails fast with
// ErrQueueFull rather than letting an over-eager source (a tight webhook loop, a
// backlog flood) exhaust the host.
type boundedQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	h       pq
	cap     int    // 0 ⇒ unbounded (default-off posture: no fence unless asked)
	nextSeq uint64 // monotonic enqueue counter; the FIFO tie-break source
	closed  bool
}

// newBoundedQueue returns a queue holding at most capacity signals. A capacity <= 0
// means unbounded — the nil-safe / default-off posture, since an operator who sets
// no bound gets a plain priority queue with no rejection path.
func newBoundedQueue(capacity int) *boundedQueue {
	q := &boundedQueue{cap: capacity}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// enqueue inserts sig, assigning it the next stable sequence under the lock so the
// FIFO tie-break is globally consistent across concurrent producers. It returns
// ErrQueueFull if the bound is reached and ErrQueueClosed after close. A successful
// enqueue signals one waiting dequeue.
func (q *boundedQueue) enqueue(sig QueuedSignal) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	if q.cap > 0 && q.h.Len() >= q.cap {
		return ErrQueueFull
	}
	sig.seq = q.nextSeq
	q.nextSeq++
	heap.Push(&q.h, sig)
	q.cond.Signal()
	return nil
}

// dequeue blocks until a signal is available, the context is cancelled, or the
// queue is closed and drained. It returns (signal, true, nil) on a real item;
// (zero, false, ctx.Err()) on cancellation; and (zero, false, nil) once the queue
// is closed AND empty (the clean drain-complete signal the daemon loops on).
//
// Cancellation is honored even while parked on the cond: a watcher goroutine
// broadcasts when ctx is done, so a blocked dequeue re-checks ctx and unparks. The
// watcher is bounded to this call's lifetime (it exits via stop).
func (q *boundedQueue) dequeue(ctx context.Context) (QueuedSignal, bool, error) {
	stop := q.wakeOnCancel(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for q.h.Len() == 0 {
		if err := ctx.Err(); err != nil {
			return QueuedSignal{}, false, err
		}
		if q.closed {
			return QueuedSignal{}, false, nil // closed and fully drained
		}
		q.cond.Wait()
	}
	// A wake can come from cancellation rather than an enqueue: re-check ctx before
	// committing to a Pop, so a cancelled drain returns the context error promptly
	// even if an item happens to be present.
	if err := ctx.Err(); err != nil {
		return QueuedSignal{}, false, err
	}
	it := heap.Pop(&q.h).(QueuedSignal)
	return it, true, nil
}

// wakeOnCancel starts a goroutine that broadcasts the cond once ctx is done, so any
// dequeue parked in cond.Wait re-evaluates ctx.Err and unparks. It returns a stop
// func that tears the goroutine down when the dequeue completes on its own, so the
// helper never outlives the call. A nil/never-cancelled ctx simply never fires.
func (q *boundedQueue) wakeOnCancel(ctx context.Context) (stop func()) {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// close marks the queue closed and wakes every waiter so blocked dequeues can drain
// the remainder and then observe the closed-and-empty terminal. Already-queued
// signals are NOT discarded — they remain dequeue-able until drained, so shutdown
// is graceful, not lossy. Idempotent.
func (q *boundedQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// len reports the current backlog depth (for tests and metrics). Held under the
// lock so it is a consistent snapshot.
func (q *boundedQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.h.Len()
}
