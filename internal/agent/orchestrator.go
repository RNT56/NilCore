// Package agent wires a task through a fresh worktree, a backend, and the
// verifier, recording every step. Phase 1 runs the configured backend once in an
// isolated worktree. The adaptive routing policy (Phase 3) slots in through the
// Router and Spawner seams below without changing the Task/Result contract or
// re-editing this package.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/project"
	"nilcore/internal/route"
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

	// RaceN, when > 1, escalates a VERIFY-FAILED single task to a best-of-N race
	// (P3-T04, internal/route): N fresh worktrees run a backend in parallel and the
	// first to pass the verifier wins (route.Race judges by the verifier — I2). It is
	// ADAPTIVE — it fires ONLY after the cheap single path fails, so easy tasks (which
	// pass first try) never pay the N× multiplier. <= 1 (the default) ⇒ no race,
	// byte-identical to before.
	RaceN int

	// Phase 5 supervision seam (P5-T01) — both optional; when unset, Execute is the
	// single-task path (BYTE-IDENTICAL to today). When Project + ShouldSupervise are
	// wired and ShouldSupervise judges the goal complex, Execute hands the goal to
	// the autonomous project loop (plan → slice → integrate → verify → reflect to a
	// verifier-green tree) instead of running it as one task. The verifier stays the
	// only authority on "done" inside the loop (I2). This supersedes the retired
	// mechanical fan-out (executePlanned): there is exactly one fan-out path.
	Project         *project.Loop
	ShouldSupervise func(goal string) bool

	// OnSuccess, if set, runs after a verified single-task completion (P4-T05),
	// so durable conventions/decisions can be written back to memory.
	OnSuccess func(ctx context.Context, t backend.Task, out Outcome)

	// Checkpoint, if set, persists task state for crash/restart durability (P6-T03).
	Checkpoint *Checkpoint
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

// Execute runs one task. When the supervision seam is wired and ShouldSupervise
// judges the goal complex, the goal is handed to the autonomous project loop;
// everything else takes the single-task path. Either way the verifier is the
// final gate. With Project==nil this is byte-identical to the single-task path.
func (o *Orchestrator) Execute(ctx context.Context, t backend.Task) (Outcome, error) {
	if o.Project != nil && o.ShouldSupervise != nil && o.ShouldSupervise(t.Goal) {
		return o.executeSupervised(ctx, t)
	}
	return o.executeSingle(ctx, t)
}

// executeSupervised hands a complex goal to the autonomous project loop, which
// drives plan → slice → integrate → verify → reflect to a verifier-green tree.
// The loop is bounded and the verifier is its only authority on done (I2); the
// single irreversible promote inside it gates through the loop's own Gate seam.
// The terminal project.Outcome is folded into the orchestrator's Outcome — Done
// is the loop's verifier verdict, never a backend self-report.
func (o *Orchestrator) executeSupervised(ctx context.Context, t backend.Task) (Outcome, error) {
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "supervise_start",
		Detail: map[string]any{"goal": t.Goal}})

	res, err := o.Project.Run(ctx)
	if err != nil {
		return Outcome{Backend: "project"}, fmt.Errorf("project loop: %w", err)
	}

	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "supervise_done",
		Detail: map[string]any{"done": res.Done, "reason": res.Reason, "iterations": res.Iterations}})

	return Outcome{
		Backend:  "project",
		Summary:  res.Summary,
		Verified: res.Done,
		Detail:   res.Reason,
	}, nil
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
	if o.Checkpoint != nil {
		_ = o.Checkpoint.Begin(ctx, t) // durable: mark running (P6-T03)
	}

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
		// A self-suspend (the `sleep` tool) is neither a completion nor a fault: do NOT
		// re-verify (the worktree is deliberately incomplete — verifying it wastes a
		// sandbox pass), and mark the task SUSPENDED (not left "running") so the restart
		// resumer skips it — the wake owns resume, so re-driving here would double it.
		// Propagate the sentinel so the session unwinds with no verdict/notification.
		if errors.Is(err, backend.ErrSuspended) {
			if o.Checkpoint != nil {
				_ = o.Checkpoint.Suspend(ctx, t.ID, t.Goal)
			}
			o.Log.Append(eventlog.Event{Task: t.ID, Backend: be.Name(), Kind: "task_suspended"})
			return Outcome{Backend: be.Name(), Summary: res.Summary}, backend.ErrSuspended
		}
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

	// Adaptive escalation: the cheap single path failed verification — race a
	// best-of-N to recover (only when RaceN > 1; easy tasks never reach here).
	if !rep.Passed && o.RaceN > 1 {
		if rout, ok := o.raceEscalate(ctx, t); ok {
			return rout, nil
		}
	}

	out := Outcome{
		Backend:  res.Backend,
		Summary:  res.Summary,
		Verified: rep.Passed,
		Detail:   rep.Output,
	}
	if o.Checkpoint != nil {
		_ = o.Checkpoint.Complete(ctx, t.ID, t.Goal, out.Verified) // durable: terminal status
	}
	if rep.Passed && o.OnSuccess != nil {
		o.OnSuccess(ctx, t, out) // write durable facts back to memory (P4-T05)
	}
	return out, nil
}

// raceEscalate runs a best-of-N race after a single-task verify failure: it cuts
// RaceN fresh worktrees off the base HEAD, runs a backend in each, and returns the
// first whose verifier passes (route.Race is the judge — I2). It is ONE-SHOT per
// task (a race never re-races) and, like the single path, report-only — the
// winning worktree is disposable. Returns (_, false) when none pass, leaving the
// caller to return the original failed Outcome.
func (o *Orchestrator) raceEscalate(ctx context.Context, t backend.Task) (Outcome, bool) {
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "race_escalate", Detail: map[string]any{"n": o.RaceN}})
	var cands []route.Candidate
	for i := 0; i < o.RaceN; i++ {
		rwt, err := worktree.CreateFrom(ctx, o.BaseRepo,
			"race/"+t.ID+"-"+strconv.Itoa(i), t.ID+"-race-"+strconv.Itoa(i), "HEAD")
		if err != nil {
			continue
		}
		defer func() { _ = rwt.Cleanup() }()
		rt := t
		rt.Dir = rwt.Path()
		renv := o.NewEnv(rt.Dir)
		cands = append(cands, route.Candidate{Backend: renv.Backend, Verifier: renv.Verifier, Task: rt})
	}
	if len(cands) == 0 {
		return Outcome{}, false
	}
	rres, ok := route.Race(ctx, cands, o.Log)
	if !ok {
		return Outcome{}, false
	}
	out := Outcome{Backend: rres.Backend, Summary: rres.Summary, Verified: true}
	if o.Checkpoint != nil {
		_ = o.Checkpoint.Complete(ctx, t.ID, t.Goal, true)
	}
	if o.OnSuccess != nil {
		o.OnSuccess(ctx, t, out)
	}
	return out, true
}
