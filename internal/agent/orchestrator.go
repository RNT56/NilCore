// Package agent wires a task through a fresh worktree, a backend, and the
// verifier, recording every step. Phase 1 runs the configured backend once in an
// isolated worktree. The adaptive routing policy (Phase 3) slots in through the
// Router and Spawner seams below without changing the Task/Result contract or
// re-editing this package.
package agent

import (
	"context"
	"fmt"

	"nilcore/internal/backend"
	"nilcore/internal/blackboard"
	"nilcore/internal/eventlog"
	"nilcore/internal/planner"
	"nilcore/internal/policy"
	"nilcore/internal/spawn"
	"nilcore/internal/verify"
	"nilcore/internal/worktree"
)

// Env is the per-task execution environment built for one worktree: the backend
// that does the work and the verifier that judges it, both pointed at the
// worktree directory. The orchestrator builds one per task via NewEnv.
type Env struct {
	Backend  backend.CodingBackend
	Verifier verify.Verifier
}

// Router selects the backend for a task. The default (SingleRouter) returns the
// configured backend unchanged; Phase 3 (P3-T04) races best-of-N and lets the
// verifier pick the winner — implemented in its own package, plugged in here.
type Router interface {
	Route(ctx context.Context, t backend.Task, def backend.CodingBackend) backend.CodingBackend
}

// Spawner runs subtasks as scoped subworkers. The default (NoSpawner) does
// nothing; Phase 3 (P3-T02) implements parallel subworkers in its own package.
type Spawner interface {
	Spawn(ctx context.Context, t backend.Task) error
}

// SingleRouter is the default Router: always the one configured backend.
type SingleRouter struct{}

// Route returns the default backend unchanged.
func (SingleRouter) Route(_ context.Context, _ backend.Task, def backend.CodingBackend) backend.CodingBackend {
	return def
}

// NoSpawner is the default Spawner: a no-op seam until Phase 3.
type NoSpawner struct{}

// Spawn does nothing in Phase 1.
func (NoSpawner) Spawn(context.Context, backend.Task) error { return nil }

// Orchestrator runs each task in a fresh git worktree of BaseRepo, then re-runs
// the project's checks as the final gate.
type Orchestrator struct {
	BaseRepo string               // git repo that worktrees are created from
	NewEnv   func(dir string) Env // builds backend + verifier for a worktree dir
	Log      *eventlog.Log
	Router   Router          // defaults to SingleRouter
	Spawner  Spawner         // defaults to NoSpawner
	Approver policy.Approver // consulted for irreversible actions; nil denies them

	// Phase 3 adaptive layer (P3-T05) — all optional; when unset, Execute is the
	// single-task path. When Plan + ShouldPlan are set and a goal is complex,
	// Execute decomposes it, runs the subtasks in parallel worktrees via RunSub,
	// records statuses on the Board, and aggregates. The verifier stays the gate
	// (each subtask verifies its own worktree through RunSub).
	Plan        func(ctx context.Context, goal string) (planner.Tree, error)
	RunSub      func(ctx context.Context, st spawn.Subtask) spawn.Result
	Board       *blackboard.Blackboard
	ShouldPlan  func(goal string) bool
	MaxParallel int

	// OnSuccess, if set, runs after a verified single-task completion (P4-T05),
	// so durable conventions/decisions can be written back to memory.
	OnSuccess func(ctx context.Context, t backend.Task, out Outcome)
}

// Gate decides whether an action may proceed right now and records the decision.
// Reversible actions auto-proceed unattended; irreversible ones (merge, push,
// deploy, payments) require the human Approver — denied by default when none is
// wired. This is the integration-boundary seam that later phases call before any
// irreversible step (P3 routing/proactivity, P5 self-edit, serve-mode channels).
func (o *Orchestrator) Gate(action string) bool {
	class := policy.Classify(action)
	allowed := policy.Gate(action, o.Approver)
	o.Log.Append(eventlog.Event{Kind: "gate", Detail: map[string]any{
		"action": action, "class": class.String(), "allowed": allowed,
	}})
	return allowed
}

// Outcome is the final, verifier-confirmed result of a task.
type Outcome struct {
	Backend  string
	Summary  string
	Verified bool   // did the project's checks pass after the backend ran?
	Detail   string // verifier output (tail) when it did not pass
}

// Execute runs one task. Complex goals are decomposed and parallelized (the
// adaptive layer); everything else takes the single-task path. Either way the
// verifier is the final gate.
func (o *Orchestrator) Execute(ctx context.Context, t backend.Task) (Outcome, error) {
	if o.Plan != nil && o.ShouldPlan != nil && o.ShouldPlan(t.Goal) {
		if out, handled, err := o.executePlanned(ctx, t); handled || err != nil {
			return out, err
		}
		// planning declined or failed — fall through to the single-task path.
	}
	return o.executeSingle(ctx, t)
}

// executeSingle runs one task: create an isolated worktree, run the backend in
// it, then re-verify as the gate. The worktree is always cleaned up.
func (o *Orchestrator) executeSingle(ctx context.Context, t backend.Task) (Outcome, error) {
	if o.NewEnv == nil {
		return Outcome{}, fmt.Errorf("orchestrator: NewEnv is required")
	}
	router := o.Router
	if router == nil {
		router = SingleRouter{}
	}

	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "task_start",
		Detail: map[string]any{"goal": t.Goal, "base_repo": o.BaseRepo}})

	wt, err := worktree.Create(ctx, o.BaseRepo, t.ID)
	if err != nil {
		return Outcome{}, fmt.Errorf("create worktree: %w", err)
	}
	defer func() {
		if cerr := wt.Cleanup(); cerr != nil {
			o.Log.Append(eventlog.Event{Task: t.ID, Kind: "worktree_cleanup",
				Detail: map[string]any{"error": cerr.Error()}})
		}
	}()

	// The task runs against the worktree, not the original repo — reversible by
	// construction.
	t.Dir = wt.Path()
	env := o.NewEnv(t.Dir)
	be := router.Route(ctx, t, env.Backend)

	o.Log.Append(eventlog.Event{Task: t.ID, Backend: be.Name(), Kind: "task_run",
		Detail: map[string]any{"worktree": wt.Path(), "branch": wt.Branch()}})

	res, err := be.Run(ctx, t)
	if err != nil {
		return Outcome{Backend: be.Name()}, fmt.Errorf("backend: %w", err)
	}

	// Source of truth: re-run the project's checks no matter which backend ran.
	// This is what makes delegating to Codex or Claude Code safe — their
	// self-report never decides whether the work ships (invariant I2).
	rep, err := env.Verifier.Check(ctx)
	if err != nil {
		return Outcome{Backend: res.Backend, Summary: res.Summary}, fmt.Errorf("final verify: %w", err)
	}
	o.Log.Append(eventlog.Event{Task: t.ID, Backend: res.Backend, Kind: "final_verify",
		Detail: map[string]any{"passed": rep.Passed}})

	out := Outcome{
		Backend:  res.Backend,
		Summary:  res.Summary,
		Verified: rep.Passed,
		Detail:   rep.Output,
	}
	if rep.Passed && o.OnSuccess != nil {
		o.OnSuccess(ctx, t, out) // write durable facts back to memory (P4-T05)
	}
	return out, nil
}
