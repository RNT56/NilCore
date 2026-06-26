package main

import (
	"context"

	"nilcore/internal/emit"
	"nilcore/internal/model"
	"nilcore/internal/scheduler"
	"nilcore/internal/session"
	"nilcore/internal/summarize"
)

// defaultServeConcurrency bounds how many serve drives run at once when the
// operator does not set -max-concurrent. Sized to a host's typical sandbox/model
// headroom; a burst of active conversations queues past it rather than overrunning.
const defaultServeConcurrency = 4

// driveGate bounds how many serve drives run SIMULTANEOUSLY across ALL threads, so
// a burst of active conversations cannot overrun the host's sandbox/model capacity.
// It uses the shared scheduler.Scheduler (a bounded FIFO worker pool) as the limiter:
// a run-func submits its work and blocks for the result, so the call stays
// synchronous while the pool caps how many execute at once. Parked (queued) drive
// goroutines are cheap; the cap is on the EXPENSIVE running work. A nil gate runs
// inline (no cap), so non-serve paths stay byte-identical.
type driveGate struct {
	sched *scheduler.Scheduler
}

// newDriveGate builds a gate capping concurrency to max (≤0 ⇒ default) and starts
// the worker pool on ctx. Call close() after the serve loop drains to stop cleanly.
func newDriveGate(ctx context.Context, max int) *driveGate {
	if max <= 0 {
		max = defaultServeConcurrency
	}
	s := scheduler.New(max)
	s.Start(ctx)
	return &driveGate{sched: s}
}

// runOutcome executes fn under the concurrency cap and returns its result. The
// drive runs on its OWN ctx (not the pool's); if that ctx is cancelled while the
// task is queued or running, runOutcome returns ctx.Err() so the parked drive
// goroutine never wedges at shutdown.
func (g *driveGate) runOutcome(ctx context.Context, id string, fn func(context.Context) (session.DriveOutcome, error)) (session.DriveOutcome, error) {
	if g == nil {
		return fn(ctx)
	}
	var out session.DriveOutcome
	var ferr error
	done := make(chan struct{})
	g.sched.Submit(scheduler.Task{ID: id, Run: func(context.Context) error {
		defer close(done)
		out, ferr = fn(ctx) // the drive's own ctx, not the pool's lifecycle ctx
		return ferr
	}})
	select {
	case <-done:
		return out, ferr
	case <-ctx.Done():
		return session.DriveOutcome{}, ctx.Err()
	}
}

// close drains in-flight drives and stops the pool (no leaked workers). Safe to
// call once, after the serve loop has returned (all submits done).
func (g *driveGate) close() {
	if g != nil {
		_ = g.sched.Wait()
	}
}

// gateNative/gateSupervise/gateProject wrap a serve run-func so its body executes
// under the gate. A nil gate returns the run-func unchanged (no wrapper overhead).

func gateNative(g *driveGate, run session.RunNativeFunc) session.RunNativeFunc {
	if g == nil {
		return run
	}
	return func(ctx context.Context, in session.NativeRun) (session.DriveOutcome, error) {
		return g.runOutcome(ctx, in.TaskID, func(c context.Context) (session.DriveOutcome, error) { return run(c, in) })
	}
}

func gateSupervise(g *driveGate, run session.RunSuperviseFunc) session.RunSuperviseFunc {
	if g == nil {
		return run
	}
	return func(ctx context.Context, goal string, seed []model.Message, in session.InboxHandle, out emit.Emitter, ask session.AskerHandle) (session.DriveOutcome, error) {
		return g.runOutcome(ctx, goal, func(c context.Context) (session.DriveOutcome, error) { return run(c, goal, seed, in, out, ask) })
	}
}

func gateProject(g *driveGate, run session.RunProjectFunc) session.RunProjectFunc {
	if g == nil {
		return run
	}
	return func(ctx context.Context, goal string, seed summarize.ContextSummary, out emit.Emitter) (session.DriveOutcome, error) {
		return g.runOutcome(ctx, goal, func(c context.Context) (session.DriveOutcome, error) { return run(c, goal, seed, out) })
	}
}
