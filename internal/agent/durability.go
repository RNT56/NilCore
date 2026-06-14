package agent

import (
	"context"

	"nilcore/internal/backend"
	"nilcore/internal/store"
)

// Checkpoint persists orchestrator task state to the store so an interrupted run
// can resume or fail cleanly on restart, and a SIGTERM can checkpoint before exit
// (P6-T03). State transitions are single UpsertTask writes, so a crash never
// leaves partial state. The orchestrator marks a task "running" at start and
// "done"/"failed" at the end when a Checkpoint is wired.
type Checkpoint struct {
	store *store.Store
}

// NewCheckpoint returns a checkpointer over the store.
func NewCheckpoint(s *store.Store) *Checkpoint { return &Checkpoint{store: s} }

// Begin records a task as running.
func (c *Checkpoint) Begin(ctx context.Context, t backend.Task) error {
	return c.store.UpsertTask(ctx, store.Task{ID: t.ID, Goal: t.Goal, Status: "running"})
}

// Complete records a task's terminal status.
func (c *Checkpoint) Complete(ctx context.Context, taskID, goal string, verified bool) error {
	status := "failed"
	if verified {
		status = "done"
	}
	return c.store.UpsertTask(ctx, store.Task{ID: taskID, Goal: goal, Status: status})
}

// Interrupt marks every running task "interrupted" — the clean SIGTERM checkpoint
// so the next start knows what to resume.
func (c *Checkpoint) Interrupt(ctx context.Context) error {
	running, err := c.store.TasksByStatus(ctx, "running")
	if err != nil {
		return err
	}
	for _, t := range running {
		if err := c.store.UpsertTask(ctx, store.Task{ID: t.ID, Goal: t.Goal, Status: "interrupted"}); err != nil {
			return err
		}
	}
	return nil
}

// InFlight returns tasks left running or interrupted by a previous process.
func (c *Checkpoint) InFlight(ctx context.Context) ([]store.Task, error) {
	var out []store.Task
	for _, status := range []string{"running", "interrupted"} {
		ts, err := c.store.TasksByStatus(ctx, status)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// Resume re-runs each in-flight task via run, recording the result. A task that
// errors on resume is marked failed cleanly (the reason is surfaced by run's
// error), so a restart never silently drops or corrupts work.
func (c *Checkpoint) Resume(ctx context.Context, run func(ctx context.Context, t backend.Task) (verified bool, err error)) error {
	inflight, err := c.InFlight(ctx)
	if err != nil {
		return err
	}
	for _, st := range inflight {
		t := backend.Task{ID: st.ID, Goal: st.Goal}
		verified, rerr := run(ctx, t)
		if rerr != nil {
			_ = c.Complete(ctx, st.ID, st.Goal, false)
			continue
		}
		if err := c.Complete(ctx, st.ID, st.Goal, verified); err != nil {
			return err
		}
	}
	return nil
}
