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

// Selector orders/filters a set of candidate backend NAMES best-first for a task
// (Phase 13 multi-backend strength-routing). It is the structural seam the Trust
// Ledger plugs into — trust.Selector satisfies this shape WITHOUT importing agent,
// exactly like trust.Router satisfies Router. I2 boundary: a Selector only orders
// WHICH backend to run first; it NEVER decides "done" or the race winner. The
// verifier still judges every race (route.Race) and re-runs as the final gate
// (executeSingle), so a Selector is a bias on the first attempt, never an override.
type Selector interface {
	Select(ctx context.Context, t backend.Task, names []string) []string
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

	// KeepBranch, when true, PRESERVES the verified worktree branch instead of the
	// default disposable cleanup: on a verified single-task success the working tree
	// is committed and the branch is Released (kept), and its name is reported in
	// Outcome.Branch — which is what lets an opt-in trigger→PR flow (D4) push the
	// verified work. false (the default) ⇒ byte-identical: the worktree and its
	// branch are cleaned up exactly as before, so no run leaves a branch behind.
	KeepBranch bool

	// Phase 13 multi-backend strength-routing (default-zero = today's single-backend
	// path, byte-identical). These three fields together unlock a path where an
	// operator who has configured several DISTINCT backends (native + codex +
	// claude-code) gets the historically-strongest one tried FIRST, and on a
	// verify-fail a race of the DISTINCT backends where the VERIFIER picks the winner
	// (the Trust Ledger only ORDERS — I2). The multi path is taken ONLY when
	// multiBackend() holds (len(Backends) > 1 AND NewEnvFor != nil); otherwise every
	// existing field and code path is untouched.

	// Backends is the set of configured backend NAMES, in operator-declared order.
	// Empty or a single name ⇒ the single-backend path (NewEnv). It is never mutated
	// by the orchestrator (orderBackends works on a copy).
	Backends []string

	// NewEnvFor builds an Env whose Backend is the NAMED one, resolved FRESH per
	// worktree directory. This is what the multi path uses instead of NewEnv: it
	// avoids a stale construction-time backend by re-resolving the named backend for
	// each worktree. Required for the multi path; nil ⇒ stay on the single path.
	NewEnvFor func(dir, backendName string) Env

	// Selector, when set, orders the candidate backend names best-first (the Trust
	// Ledger plugs in here). nil ⇒ the configured Backends order is used as-is. A
	// Selector only orders WHICH backend to run; the verifier still decides "done"
	// (I2).
	Selector Selector
}

// multiBackend reports whether the Phase-13 multi-backend path is active: more than
// one configured backend name AND a NewEnvFor to resolve each by name. With either
// unset (the default) this is false and executeSingle/raceEscalate behave EXACTLY
// as before (byte-identical).
func (o *Orchestrator) multiBackend() bool {
	return len(o.Backends) > 1 && o.NewEnvFor != nil
}

// orderBackends returns the candidate backend names best-first for this task. When
// a Selector is wired it orders a COPY of o.Backends (the Trust Ledger biases toward
// the historically-strongest); otherwise the configured order is kept. o.Backends is
// never mutated. The result is de-duplicated and empty names are dropped, so a
// fresh worktree is built once per DISTINCT backend.
func (o *Orchestrator) orderBackends(ctx context.Context, t backend.Task) []string {
	src := make([]string, len(o.Backends))
	copy(src, o.Backends)
	if o.Selector != nil {
		src = o.Selector.Select(ctx, t, src)
	}
	seen := make(map[string]bool, len(src))
	out := make([]string, 0, len(src))
	for _, n := range src {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
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
	Branch   string // verified branch name, set only when KeepBranch preserved it (D4); else ""
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
	// The single path needs NewEnv; the multi path (multiBackend) supplies NewEnvFor
	// instead. multiBackend() already guarantees NewEnvFor != nil, so this guard only
	// fires when neither builder is wired — and on the single path it is byte-identical.
	if o.NewEnv == nil && !o.multiBackend() {
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
	// keepBranch is flipped on only for a verified success under KeepBranch (D4):
	// then the worktree is Released (its branch kept for a PR push) rather than
	// Cleanup'd. Every other exit — error, suspend, verify-fail, or the default
	// disposable mode — deletes the worktree and its branch, byte-identical to before.
	keepBranch := false
	defer func() {
		if keepBranch {
			if rerr := wt.Release(); rerr != nil {
				o.Log.Append(eventlog.Event{Task: t.ID, Kind: "worktree_release",
					Detail: map[string]any{"error": rerr.Error()}})
			}
			return
		}
		if cerr := wt.Cleanup(); cerr != nil {
			o.Log.Append(eventlog.Event{Task: t.ID, Kind: "worktree_cleanup",
				Detail: map[string]any{"error": cerr.Error()}})
		}
	}()

	// The task runs against the worktree, not the original repo — reversible by
	// construction.
	t.Dir = wt.Path()
	// Backend SELECTION — the ONLY thing the multi-backend path changes here. The
	// verifier (env.Verifier, backend-independent) and everything after are identical
	// on both paths (I2: this only orders WHICH backend runs first).
	var env Env
	var be backend.CodingBackend
	if o.multiBackend() {
		// Multi path: pick the historically-strongest configured backend first
		// (Selector orders; nil ⇒ configured order) and resolve it FRESH for this
		// worktree via NewEnvFor (no stale construction-time backend).
		names := o.orderBackends(ctx, t)
		env = o.NewEnvFor(t.Dir, names[0])
		be = env.Backend
		order := "configured"
		if o.Selector != nil {
			order = "trust"
		}
		o.Log.Append(eventlog.Event{Task: t.ID, Backend: be.Name(), Kind: "backend_select",
			Detail: map[string]any{"chosen": names[0], "order": names, "by": order}})
	} else {
		// Single path — UNCHANGED, byte-identical to before.
		env = o.NewEnv(t.Dir)
		be = router.Route(ctx, t, env.Backend)
	}

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

	// Adaptive escalation: the cheap single path failed verification — race to
	// recover. On the single path this is best-of-N copies of the one backend, gated
	// on RaceN > 1 exactly as before (byte-identical). On the multi path it races the
	// DISTINCT configured backends and the gate is multiBackend() itself (more than
	// one backend is already guaranteed), so an operator gets the cross-backend race
	// without also having to set RaceN. Easy tasks (passed first try) never reach here.
	if !rep.Passed && (o.RaceN > 1 || o.multiBackend()) {
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
	// D4: preserve the verified branch for an opt-in trigger→PR push. Commit any
	// uncommitted verified working-tree state (best-effort — the loop may have
	// committed already), keep the branch, and report its name. Gated on KeepBranch
	// so the default path is byte-identical (no commit, branch cleaned up).
	if o.KeepBranch && rep.Passed {
		// Commit any uncommitted verified state (the loop may already have committed,
		// in which case this is a clean no-op). A genuine commit FAILURE is logged so
		// a later empty/dangling PR is auditable; we still preserve the branch — the
		// loop's own commits may carry the work, and the PR flow re-checks the diff.
		if _, _, cerr := wt.Commit(ctx, "nilcore: "+t.Goal); cerr != nil {
			o.Log.Append(eventlog.Event{Task: t.ID, Kind: "keep_branch_commit",
				Detail: map[string]any{"error": cerr.Error()}})
		}
		keepBranch = true
		out.Branch = wt.Branch()
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
	var cands []route.Candidate
	if o.multiBackend() {
		// Multi path: race the DISTINCT configured backends (one fresh worktree per
		// ordered name) so route.Race competes DIFFERENT backends and the verifier
		// picks the winner; best-first ordering breaks verifier ties toward the
		// historically-strongest. The per-candidate race_outcome events now carry
		// distinct backends — the signal that closes the Trust Ledger loop.
		names := o.orderBackends(ctx, t)
		o.Log.Append(eventlog.Event{Task: t.ID, Kind: "race_escalate",
			Detail: map[string]any{"n": len(names), "backends": names}})
		for i, name := range names {
			rwt, err := worktree.CreateFrom(ctx, o.BaseRepo,
				"race/"+t.ID+"-"+strconv.Itoa(i), t.ID+"-race-"+strconv.Itoa(i), "HEAD")
			if err != nil {
				continue
			}
			defer func() { _ = rwt.Cleanup() }()
			rt := t
			rt.Dir = rwt.Path()
			renv := o.NewEnvFor(rt.Dir, name)
			cands = append(cands, route.Candidate{Backend: renv.Backend, Verifier: renv.Verifier, Task: rt})
		}
		if len(cands) == 0 {
			return Outcome{}, false
		}
		return o.runRace(ctx, t, cands)
	}

	// Single path — UNCHANGED, byte-identical: RaceN copies of the one backend.
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "race_escalate", Detail: map[string]any{"n": o.RaceN}})
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
	return o.runRace(ctx, t, cands)
}

// runRace is the shared tail of both raceEscalate paths: judge the candidates by
// the verifier (route.Race — I2), and on a winner record the durable terminal
// status and write back facts. Both the single (N-copies) and multi (distinct
// backends) paths feed identical candidates here, so the single path stays
// byte-identical — only the candidate SET differs between them.
func (o *Orchestrator) runRace(ctx context.Context, t backend.Task, cands []route.Candidate) (Outcome, bool) {
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
