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
	"strings"

	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/project"
	"nilcore/internal/route"
	"nilcore/internal/sandbox"
	"nilcore/internal/trust"
	"nilcore/internal/verify"
	"nilcore/internal/worktree"
)

// Env is the per-task execution environment built for one worktree: the backend
// that does the work and the verifier that judges it, both pointed at the
// worktree directory. The orchestrator builds one per task via NewEnv.
type Env struct {
	Backend  backend.CodingBackend
	Verifier verify.Verifier
	// Box is the worktree sandbox the backend/verifier run against. It is exposed so
	// the optional SelfAccept hook can run the agent's gated acceptance checks inside
	// the SAME box (I4) after the floor verifier passes. May be nil (paths that do
	// not wire self-acceptance leave it unset; the hook is then never called).
	Box sandbox.Sandbox
}

// SelfAcceptFunc is the optional closed-loop self-acceptance hook (internal/verify/
// selfacc). After the project's verifier (the floor — I2) passes, the orchestrator
// calls it so the agent's OWN gated acceptance checks must ALSO pass before the run
// is judged done. It receives the goal (untrusted data — I7), the worktree box (to
// run checks inside the sandbox — I4), a structured-gate closure (so each proposed
// check is approved like any boundary action — attended human / headless deny /
// graduated auto-approval), and the log (audit — I5). It returns whether the self-
// authored acceptance bar held, plus a bounded detail for the verdict output. It can
// only ADD to the bar: the orchestrator consults it ONLY when the floor is green and
// a false result reddens the verdict — it can never green a red floor.
type SelfAcceptFunc func(ctx context.Context, goal string, box sandbox.Sandbox, gate func(policy.GateAction) bool, log *eventlog.Log) (passed bool, detail string)

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
// Ledger plugs into — trust.Selector satisfies this shape WITHOUT importing agent
// (the leaf rule: the orchestrator wires the leaf, never the reverse). I2 boundary: a Selector only orders
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

	// SelfAccept, when set, is the closed-loop self-acceptance hook (opt-in). After
	// the floor verifier passes, the agent's own gated acceptance checks must ALSO
	// pass (it can only ADD to the bar — I2). nil ⇒ never called (byte-identical).
	SelfAccept SelfAcceptFunc

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

	// Oracle, when set, is the Phase-16 trust-informed routing seam (RTE-T05): it
	// ORDERS / PRUNES the candidate names per task-class and SIZES the verify-fail
	// escalation (best-of-N, attempt budget) from earned, verifier-judged evidence.
	// It is consulted AFTER the Selector, through the nil-safe PlanRoute / OracleRaceN
	// helpers, so a nil Oracle (the default) is byte-identical to the pre-RTE static
	// path. Like the Selector it only biases WHAT to attempt and HOW HARD — the
	// verifier still judges every race (route.Race) and re-runs as the final gate
	// (I2: the oracle never decides "done" and never picks a race winner). A
	// degenerate plan (empty candidate list) falls back to the configured set, so the
	// oracle can never starve the hot path of a runnable backend.
	Oracle TrustOracle

	// Cost, when set, returns the metered $-cost of attempting this task class with a
	// given backend (typically a meter.Pricer-derived estimate). It is recorded —
	// alongside the task class — on the race events so trust.Replay can fold the
	// per-(class, backend) cost cell that makes cost-aware routing LEARN (RTE-T06). It
	// is METADATA ONLY (I7) and never gates: nil ⇒ no cost dimension is recorded,
	// byte-identical to before. The oracle, not the model, ever sees a cost.
	Cost func(taskClass, backendName string) float64
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
// the historically-strongest); then, when an Oracle is wired, PlanRoute reorders /
// prunes the result per task-class (RTE-T05). o.Backends is never mutated. The
// result is de-duplicated and empty names are dropped, so a fresh worktree is built
// once per DISTINCT backend.
//
// Both extra stages are nil-safe and order-preserving when unwired: with Selector
// and Oracle both nil this is the configured order, byte-identical to before.
func (o *Orchestrator) orderBackends(ctx context.Context, t backend.Task) []string {
	src := make([]string, len(o.Backends))
	copy(src, o.Backends)
	if o.Selector != nil {
		src = o.Selector.Select(ctx, t, src)
	}
	// Trust-informed routing (RTE-T05): consult the possibly-nil oracle for this
	// task class. A nil oracle returns src unchanged (PlanRoute reports applied=false),
	// so the static path is untouched. The oracle only ORDERS / PRUNES — the verifier
	// still judges every race (I2).
	if plan, applied := PlanRoute(ctx, o.Oracle, trust.Classify(t.Goal), src); applied {
		src = plan.Candidates
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
	// A Selector/Oracle is documented as ordering AND filtering, so it may legitimately
	// return fewer — or, pathologically, zero — names. The multi-backend path needs at
	// least one runnable backend (executeSingle indexes [0]; raceEscalate needs
	// candidates), and o.Backends is non-empty by construction (multiBackend requires
	// len>1). So if everything was dropped, fall back to the configured set (de-duped)
	// rather than hand the caller an empty slice — defending the hot path in ONE place.
	if len(out) == 0 {
		seen = make(map[string]bool, len(o.Backends))
		for _, n := range o.Backends {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
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
		if o.Selector != nil || o.Oracle != nil {
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
	//
	// RTE-T05: a wired Oracle may SIZE the best-of-N for this task class through the
	// nil-safe OracleRaceN helper (a nil oracle returns o.RaceN unchanged — the static
	// gate, byte-identical). The oracle only sizes candidacy; the verifier still judges
	// the race (I2). raceN is recomputed once and passed to raceEscalate so the gate
	// and the race agree on N.
	raceN := OracleRaceN(o.Oracle, trust.Classify(t.Goal), o.RaceN)
	if !rep.Passed && (raceN > 1 || o.multiBackend()) {
		if rout, ok := o.raceEscalate(ctx, t, raceN); ok {
			return rout, nil
		}
	}

	// Closed-loop self-acceptance (opt-in, P16): once the project's verifier (the
	// floor — I2) is GREEN, the agent's OWN gated acceptance checks must ALSO pass
	// before the run is judged done. The hook is consulted ONLY when rep.Passed, and
	// a false result reddens the verdict — so it can only ever RAISE the bar, never
	// green a red floor. Each proposed check is gated (attended human / headless deny
	// / graduated auto-approval) and runs inside the worktree box (I4). nil hook ⇒
	// byte-identical. (Raced winners take the early return above and skip this extra
	// bar — a deliberate, safe scoping: the floor still governed them.)
	if rep.Passed && o.SelfAccept != nil {
		gate := func(a policy.GateAction) bool { return policy.GateStructured(a, o.Approver) }
		if saPassed, saDetail := o.SelfAccept(ctx, t.Goal, env.Box, gate, o.Log); !saPassed {
			rep.Passed = false
			rep.Output = strings.TrimSpace(rep.Output + "\nself-acceptance: " + saDetail)
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
// raceN fresh worktrees off the base HEAD, runs a backend in each, and returns the
// first whose verifier passes (route.Race is the judge — I2). It is ONE-SHOT per
// task (a race never re-races) and, like the single path, report-only — the
// winning worktree is disposable. Returns (_, false) when none pass, leaving the
// caller to return the original failed Outcome.
//
// raceN is the (possibly oracle-sized) best-of-N for the single path; the multi
// path always races the DISTINCT configured backends and ignores raceN. With a nil
// Oracle raceN == o.RaceN, so the single path is byte-identical to before.
func (o *Orchestrator) raceEscalate(ctx context.Context, t backend.Task, raceN int) (Outcome, bool) {
	// RTE-T05 learning dimensions. class + cost are recorded on the race_escalate
	// event ONLY when routing/cost is wired (Oracle or Cost set), so the default-off
	// path stays byte-identical. They are metadata (I7) the model never sees; the
	// verifier still judges the race (I2).
	class := ""
	if o.Oracle != nil || o.Cost != nil {
		class = trust.Classify(t.Goal)
	}
	var cands []route.Candidate
	if o.multiBackend() {
		// Multi path: race the DISTINCT configured backends (one fresh worktree per
		// ordered name) so route.Race competes DIFFERENT backends and the verifier
		// picks the winner; best-first ordering breaks verifier ties toward the
		// historically-strongest. The per-candidate race_outcome events now carry
		// distinct backends — the signal that closes the Trust Ledger loop.
		names := o.orderBackends(ctx, t)
		o.Log.Append(eventlog.Event{Task: t.ID, Kind: "race_escalate",
			Detail: o.raceEscalateDetail(class, map[string]any{"n": len(names), "backends": names}, names)})
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
			rc := route.Candidate{Backend: renv.Backend, Verifier: renv.Verifier, Task: rt, Class: class}
			if o.Cost != nil {
				rc.Cost = o.Cost(class, name)
			}
			cands = append(cands, rc)
		}
		if len(cands) == 0 {
			return Outcome{}, false
		}
		return o.runRace(ctx, t, cands)
	}

	// Single path — byte-identical when the oracle is unwired: raceN copies of the
	// one backend (raceN == o.RaceN with a nil Oracle).
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "race_escalate",
		Detail: o.raceEscalateDetail(class, map[string]any{"n": raceN}, nil)})
	for i := 0; i < raceN; i++ {
		rwt, err := worktree.CreateFrom(ctx, o.BaseRepo,
			"race/"+t.ID+"-"+strconv.Itoa(i), t.ID+"-race-"+strconv.Itoa(i), "HEAD")
		if err != nil {
			continue
		}
		defer func() { _ = rwt.Cleanup() }()
		rt := t
		rt.Dir = rwt.Path()
		renv := o.NewEnv(rt.Dir)
		cands = append(cands, route.Candidate{Backend: renv.Backend, Verifier: renv.Verifier, Task: rt, Class: class})
	}
	if len(cands) == 0 {
		return Outcome{}, false
	}
	return o.runRace(ctx, t, cands)
}

// raceEscalateDetail enriches a race_escalate event's Detail with the RTE-T05
// learning dimensions — the task class and, when a Cost func is wired, the metered
// per-candidate $-cost — so the routing evidence is recorded ALONGSIDE the verdict
// the race produces. It is the in-scope home for class/cost: route.Race owns the
// per-candidate race_outcome event (out of this task's owned set), so the dimensions
// ride the orchestrator-owned race_escalate event that frames the same race.
//
// DEFAULT-OFF: an empty class (Oracle and Cost both nil) leaves base untouched and
// returned verbatim — byte-identical to the pre-RTE event. cost is added only when a
// Cost func is wired AND backend names are known (the multi path). The values are
// metadata, never instructions (I7), and never gate (I2).
func (o *Orchestrator) raceEscalateDetail(class string, base map[string]any, names []string) map[string]any {
	if class == "" {
		return base
	}
	base["class"] = class
	if o.Cost != nil && len(names) > 0 {
		costs := make(map[string]float64, len(names))
		for _, n := range names {
			costs[n] = o.Cost(class, n)
		}
		base["cost"] = costs
	}
	return base
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
