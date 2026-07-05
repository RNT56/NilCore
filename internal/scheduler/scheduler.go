// Package scheduler runs many tasks concurrently under a fixed concurrency cap
// with fair (FIFO) ordering and backpressure. The orchestrator can fan out work
// (P6) without letting an unbounded number of backends, sandboxes, or model
// calls run at once: excess tasks queue instead of overrunning the cap, and a
// caller can block until the whole queue has drained.
//
// The design is deliberately tiny — an UNBOUNDED internal FIFO queue (a slice
// guarded by a mutex + condition variable) feeding a worker pool sized to
// maxConcurrent. The queue is unbounded on purpose: every production caller
// submits ALL ready work BEFORE calling Start (internal/spawn runWave,
// internal/swarm runFlat), so no worker drains during Submit. A fixed-size buffer
// would deadlock the (buffer+1)-th pre-Start Submit forever; an unbounded queue
// makes Submit non-blocking for any pre-Start volume while the cap on *running*
// tasks is still enforced by the worker pool, not the queue depth. The only state
// worth trusting is the concurrency invariant, which the tests assert with the
// -race detector and atomic counters.
package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Task is one unit of work. Run receives the scheduler's context and must honor
// its cancellation; a returned error is recorded but never stops the pool (one
// task's failure must not starve the others).
type Task struct {
	ID  string
	Run func(ctx context.Context) error
}

// Scheduler is an unbounded FIFO queue fronting a bounded worker pool. The zero
// value is not usable; construct one with New.
type Scheduler struct {
	maxConcurrent int

	startOnce sync.Once
	wg        sync.WaitGroup // tracks in-flight + queued tasks until drained

	inFlight atomic.Int64 // currently running
	maxSeen  atomic.Int64 // high-water mark of inFlight
	ran      atomic.Int64 // tasks that completed Run

	// qmu guards the FIFO queue and the closed flag; qcond wakes a worker when a
	// task is enqueued or the queue is closed. This unbounded slice-backed queue
	// replaces a fixed-size channel so Submit never blocks the producer, however
	// many tasks are submitted before Start (the submit-all-then-Start pattern).
	qmu    sync.Mutex
	qcond  *sync.Cond
	queue  []Task // FIFO: append to the tail, pop from the head
	closed bool   // set by Wait once the final task has drained; no more enqueues

	mu   sync.Mutex
	errs []error // first errors returned by tasks, in completion order
}

// New returns a Scheduler that runs at most maxConcurrent tasks at once. A
// maxConcurrent below 1 is clamped to 1.
func New(maxConcurrent int) *Scheduler {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	s := &Scheduler{maxConcurrent: maxConcurrent}
	s.qcond = sync.NewCond(&s.qmu)
	return s
}

// Submit enqueues t. It is safe to call before or after Start, and from many
// goroutines, at ANY volume — the queue is unbounded, so Submit never blocks the
// producer (the running-task cap is the worker pool's job, not the queue depth).
// Submit must not be called after Wait has returned.
func (s *Scheduler) Submit(t Task) {
	s.wg.Add(1)
	s.qmu.Lock()
	s.queue = append(s.queue, t)
	s.qmu.Unlock()
	s.qcond.Signal()
}

// Start begins processing with maxConcurrent workers. It is idempotent — only
// the first call spins up the pool — and returns immediately; use Wait to block
// for the drain. The ctx is passed to every Task.Run and, when cancelled,
// short-circuits queued tasks (their Run is skipped) so Wait can return.
func (s *Scheduler) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		for i := 0; i < s.maxConcurrent; i++ {
			go s.worker(ctx)
		}
	})
}

// worker pulls tasks FIFO and runs them, tracking the in-flight high-water mark.
// It blocks on the condition variable while the queue is empty and exits only once
// the queue is both closed (Wait ran) AND drained, so no task is dropped.
func (s *Scheduler) worker(ctx context.Context) {
	for {
		s.qmu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.qcond.Wait()
		}
		if len(s.queue) == 0 && s.closed {
			s.qmu.Unlock()
			return
		}
		t := s.queue[0]
		s.queue = s.queue[1:]
		s.qmu.Unlock()
		s.run(ctx, t)
	}
}

// run executes a single task with the concurrency bookkeeping. It always calls
// wg.Done so a drain completes even when a task is skipped or panics-free errs.
func (s *Scheduler) run(ctx context.Context, t Task) {
	defer s.wg.Done()

	// If the context is already cancelled, skip the body but still account for
	// the task so Wait can drain.
	if ctx.Err() != nil {
		s.recordErr(ctx.Err())
		return
	}

	cur := s.inFlight.Add(1)
	for {
		// Raise the high-water mark to at least cur, retrying on races.
		old := s.maxSeen.Load()
		if cur <= old || s.maxSeen.CompareAndSwap(old, cur) {
			break
		}
	}

	var err error
	if t.Run != nil {
		err = t.Run(ctx)
	}

	s.inFlight.Add(-1)
	s.ran.Add(1)
	s.recordErr(err)
}

func (s *Scheduler) recordErr(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}

// Wait blocks until every submitted task has finished (or been skipped on
// cancellation), then closes the queue and wakes every idle worker so they exit.
// It must be called exactly once, after the final Submit. The returned error joins
// every error returned by a task (nil if all succeeded).
func (s *Scheduler) Wait() error {
	s.wg.Wait()

	// Mark the queue closed and Broadcast so every worker blocked in qcond.Wait
	// observes the close and returns (the queue is already drained — wg.Wait only
	// returns once every submitted task completed run, and run pops before it runs).
	s.qmu.Lock()
	s.closed = true
	s.qmu.Unlock()
	s.qcond.Broadcast()

	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.errs...)
}

// MaxInFlight reports the high-water mark of simultaneously running tasks
// observed so far. After Wait it is the peak concurrency for the whole run, and
// it is guaranteed never to exceed maxConcurrent.
func (s *Scheduler) MaxInFlight() int {
	return int(s.maxSeen.Load())
}

// Ran reports how many tasks have completed Run (skipped tasks are not counted).
func (s *Scheduler) Ran() int {
	return int(s.ran.Load())
}
