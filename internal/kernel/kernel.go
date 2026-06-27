// Package kernel is the unified orchestration primitive (Phase 16, Pillar 8 — UOK,
// docs/ROADMAP-KERNEL.md). It is the single recursive engine that the three legacy
// machines — the single-task orchestrator (FLAT), the project loop, and the verified
// swarm (both DECOMPOSE) — collapse onto: ONE `Run` over a `Node`, which per a
// `Granularity` policy either runs the node as a single task or fans it out into
// children, integrates them, and re-verifies the integrated tip. `run`/`build`/`swarm`
// become `Envelope` PRESETS; the conversational router picks an envelope, not a machine.
//
// WHY this shape keeps all seven invariants:
//
//   - I1 (frozen backend contract): the kernel imports NO backend/agent/session/project/
//     swarm package — it is a pure leaf (deps_test.go). The machines plug in as INJECTED
//     RunFunc/Plan/Integrate closures the cmd layer wires; the kernel never names them.
//   - I2 (the verifier is the sole authority on "done"): the kernel NEVER marks a node
//     verified. `Outcome.Verified` comes only from the injected runner's verifier verdict.
//     The DECOMPOSE path re-runs Integrate AFTER the children — and Integrate MUST
//     re-verify the integrated tip even when every child verified (the review's I2 fix),
//     because green children can still integrate red. The kernel cannot ship anything.
//   - I5 (append-only log): the kernel appends nothing itself; the injected runners own
//     their event sequences, so a kernel-routed run is event-for-event the legacy run
//     (proven by the equivalence harness, UOK-T09).
//
// Recursion is the engine's reason to exist: `Recursive` drives the fan-out FROM THE
// kernel — plan a node, run EACH child back through `Run` (so a child may itself
// decompose), then integrate. It is BOUNDED by `MaxDepth` (default 1, which reproduces
// the legacy one-level fan-out exactly; >1 is the new capability). The cmd cutover may
// instead inject a MONOLITHIC `Decompose` that wraps a proven machine (the project loop
// / swarm controller) as one opaque call — so the cutover is byte-identical and the
// recursive engine is available, tested, and opt-in.
package kernel

import (
	"context"
	"errors"
	"fmt"
)

// Branch is the granularity decision for a Node.
type Branch int

const (
	// Flat runs the node as a single task (the orchestrator's single-task shape).
	Flat Branch = iota
	// Decompose fans the node out into children, integrates them, and re-verifies the
	// integrated tip (the project-loop / swarm shape).
	Decompose
)

func (b Branch) String() string {
	switch b {
	case Decompose:
		return "decompose"
	default:
		return "flat"
	}
}

// DefaultMaxDepth bounds recursion when an Envelope sets none. 1 reproduces the legacy
// machines' single-level fan-out exactly (a decomposed node's children run FLAT), so a
// kernel-routed run at the default depth is equivalent to today; >1 is the new recursive
// capability (a child may itself decompose).
const DefaultMaxDepth = 1

// ErrNoFlat is returned by Run when an Envelope has no Flat runner — every preset must be
// able to run a node as a single task (the irreducible base case of the recursion).
var ErrNoFlat = errors.New("kernel: envelope has no Flat runner")

// Node is one unit of work the kernel runs. It carries only structural data (never a
// secret or policy — I3); the injected runners own all execution.
type Node struct {
	// ID is the stable task id (worktree/branch naming, event correlation).
	ID string
	// Goal is the operator/parent-authored work statement. Inert data (I7).
	Goal string
	// Depth is the recursion depth; the root is 0, each Decompose increments it for its
	// children. The kernel bounds it by the envelope's MaxDepth.
	Depth int
}

// Outcome is the verifier-confirmed result of running a Node. It mirrors the legacy
// agent/project/swarm outcomes so the cmd layer folds it back unchanged. Verified is the
// VERIFIER's verdict (I2) — the kernel never sets it from anything but a runner's return.
type Outcome struct {
	Backend  string
	Summary  string
	Verified bool
	Detail   string
	Branch   string
}

// RunFunc runs a Node to a verifier-judged Outcome. The injected FLAT and (monolithic)
// DECOMPOSE runners the cmd layer wires over the proven machines have this shape, so the
// kernel never imports them.
type RunFunc func(ctx context.Context, n Node) (Outcome, error)

// Plan splits a Node into child Nodes for the recursive DECOMPOSE engine. It is a model-
// shaped step, injected; a leaf with no actionable children returns an empty slice (the
// node then has nothing to integrate and the caller treats it as a no-op decompose).
type Plan func(ctx context.Context, n Node) ([]Node, error)

// Integrate folds the children's Outcomes into the parent's tip Outcome. It MUST re-run
// the verifier on the integrated tip (I2) — green children can integrate to a red tip, so
// a parent is "done" only when the verifier passes on the merged result, never because
// every child passed. Injected.
type Integrate func(ctx context.Context, n Node, children []Outcome) (Outcome, error)

// NodeEvent is one node-boundary signal the recursive engine emits to an Observer. It is
// pure structural data (I7) for AUDIT/RESUME only — never a control signal.
type NodeEvent struct {
	// Phase is "start" (a child is about to run) or "done" (a child finished).
	Phase string
	// Node is the child node the event is about.
	Node Node
	// Outcome is the child's result, populated on a successful "done".
	Outcome Outcome
	// Err is non-empty on a "done" whose child run errored.
	Err string
}

// Observer receives node-boundary events from Recursive (a child starting, a child
// finishing) so the cmd layer can record the recursion tree to the append-only log
// (I5) for audit + resume. It is OPTIONAL (nil ⇒ silent) and injected on the Envelope
// so the kernel stays a pure leaf (it never imports eventlog). It NEVER influences
// control flow — the kernel emits and moves on, ignoring any side effect.
type Observer interface {
	OnNode(ctx context.Context, ev NodeEvent)
}

// emitNode delivers a node event to a possibly-nil Observer (the one nil-guard site).
func emitNode(ctx context.Context, obs Observer, ev NodeEvent) {
	if obs != nil {
		obs.OnNode(ctx, ev)
	}
}

// Granularity decides whether a Node runs Flat or Decomposes for a given Envelope. It
// generalizes the conversational router's machine-pick into one extensible policy: the
// router picks an ENVELOPE, the envelope's Granularity picks the BRANCH.
type Granularity interface {
	Decide(ctx context.Context, n Node, env Envelope) Branch
}

// GranularityFunc adapts a func to Granularity.
type GranularityFunc func(ctx context.Context, n Node, env Envelope) Branch

// Decide implements Granularity.
func (f GranularityFunc) Decide(ctx context.Context, n Node, env Envelope) Branch {
	return f(ctx, n, env)
}

// AlwaysFlat never decomposes — the policy for a single-task preset (e.g. plain `run`).
var AlwaysFlat Granularity = GranularityFunc(func(context.Context, Node, Envelope) Branch { return Flat })

// AlwaysDecompose always decomposes when a Decompose runner is present and depth allows —
// the policy for a preset that is decomposition by definition (e.g. `build`, `swarm`).
var AlwaysDecompose Granularity = GranularityFunc(func(context.Context, Node, Envelope) Branch { return Decompose })

// Envelope is a named PRESET: which branches it admits, how it picks between them, the
// recursion bound, and the injected runners that do the work. `run`/`build`/`swarm` are
// envelopes. The zero value is unusable (Run requires a Flat runner).
type Envelope struct {
	// Name labels the preset (run|build|swarm|…) for legibility + audit.
	Name string
	// Flat runs a node as a single task. REQUIRED — the recursion's base case.
	Flat RunFunc
	// Decompose runs a node by fanning out. Optional: nil ⇒ a flat-only preset. For the
	// cutover it wraps a proven machine (project loop / swarm) as one opaque call; for a
	// recursive preset it is Recursive(...), which drives the fan-out from the kernel.
	Decompose RunFunc
	// Granularity picks Flat vs Decompose. nil ⇒ AlwaysFlat (never decomposes).
	Granularity Granularity
	// MaxDepth bounds recursion. <=0 ⇒ DefaultMaxDepth. At depth >= MaxDepth a node runs
	// Flat regardless of Granularity, so recursion can never run away.
	MaxDepth int
	// MaxChildren bounds the fan-out WIDTH: a Plan returning more than this many children
	// fails the decompose (fail-closed), so a runaway plan can never spawn an unbounded
	// fan-out. <=0 ⇒ unbounded (the prior behaviour). Together with MaxDepth this caps the
	// total recursive work at MaxChildren^MaxDepth.
	MaxChildren int
	// Observer receives node-boundary events (start/done) from Recursive for audit +
	// resume (I5). nil ⇒ silent. It never influences control flow.
	Observer Observer
}

// maxDepth resolves the effective recursion bound.
func (env Envelope) maxDepth() int {
	if env.MaxDepth <= 0 {
		return DefaultMaxDepth
	}
	return env.MaxDepth
}

// branchFor decides how node n runs under this envelope, applying the depth + capability
// guards before consulting the Granularity policy. A node can only decompose when the
// envelope HAS a Decompose runner AND a Granularity AND the depth bound still admits it.
func (env Envelope) branchFor(ctx context.Context, n Node) Branch {
	if env.Decompose == nil || env.Granularity == nil {
		return Flat
	}
	if n.Depth >= env.maxDepth() {
		return Flat // depth bound — children of the deepest level run as single tasks
	}
	return env.Granularity.Decide(ctx, n, env)
}

// Run is the unified primitive: dispatch node n to the envelope's Flat or Decompose
// branch per its Granularity (bounded by MaxDepth). It is the single entry the four
// entrypoints route through under the cutover. The kernel adds NO behaviour of its own
// beyond the dispatch + bound — the injected runners own execution, verification (I2),
// gating (I3), and the event log (I5), so a kernel-routed run is the legacy run.
func Run(ctx context.Context, env Envelope, n Node) (Outcome, error) {
	if env.Flat == nil {
		return Outcome{}, ErrNoFlat
	}
	// Deliberately NO ctx.Err() short-circuit here: the injected runner (the wrapped
	// machine) owns context handling, and the legacy machines emit their opening event
	// (task_start / project_start / a finish outcome) BEFORE honoring a cancelled ctx —
	// so short-circuiting here would diverge from the legacy run on a pre-cancelled
	// context (the equivalence the cutover rests on). The kernel passes ctx straight
	// through; the per-child ctx check in Recursive is the kernel's OWN loop guard
	// (a new code path with no legacy equivalent).
	if env.branchFor(ctx, n) == Decompose {
		return env.Decompose(ctx, n)
	}
	return env.Flat(ctx, n)
}

// Recursive builds a Decompose RunFunc whose fan-out is driven BY THE KERNEL: it plans
// the node into children, runs EACH child back through Run with the SAME envelope (so a
// child may itself decompose — the recursion the engine exists for), then Integrates the
// children into the parent tip. Integrate MUST re-verify the integrated tip (I2). This is
// the kernel's native recursive engine; the cmd cutover may instead inject a monolithic
// Decompose that wraps a proven machine.
//
// The envelope is taken by POINTER so the returned func recurses with the FINAL envelope
// (the caller sets env.Decompose = Recursive(&env, plan, integrate) after declaring env).
// A child run error aborts the decompose (the integrator never sees a partial set it did
// not produce); a child that returns unverified is still passed to Integrate, which is
// the sole judge of whether the integrated tip is done (I2).
func Recursive(env *Envelope, plan Plan, integrate Integrate) RunFunc {
	return func(ctx context.Context, n Node) (Outcome, error) {
		if plan == nil || integrate == nil {
			return Outcome{}, fmt.Errorf("kernel: recursive decompose for %q needs both Plan and Integrate", n.ID)
		}
		children, err := plan(ctx, n)
		if err != nil {
			return Outcome{}, fmt.Errorf("kernel: planning %q: %w", n.ID, err)
		}
		// Fail-closed width bound: a runaway plan can never spawn an unbounded fan-out.
		if env.MaxChildren > 0 && len(children) > env.MaxChildren {
			return Outcome{}, fmt.Errorf("kernel: plan for %q returned %d children, exceeds MaxChildren=%d", n.ID, len(children), env.MaxChildren)
		}
		outs := make([]Outcome, 0, len(children))
		for _, c := range children {
			if err := ctx.Err(); err != nil {
				return Outcome{}, err
			}
			c.Depth = n.Depth + 1 // descend — bounded by env.MaxDepth inside Run
			emitNode(ctx, env.Observer, NodeEvent{Phase: "start", Node: c})
			o, err := Run(ctx, *env, c)
			if err != nil {
				emitNode(ctx, env.Observer, NodeEvent{Phase: "done", Node: c, Err: err.Error()})
				return Outcome{}, fmt.Errorf("kernel: running child %q of %q: %w", c.ID, n.ID, err)
			}
			emitNode(ctx, env.Observer, NodeEvent{Phase: "done", Node: c, Outcome: o})
			outs = append(outs, o)
		}
		// Integrate re-verifies the tip (I2): a parent is verified only if the merged
		// result passes the verifier, never because every child did.
		return integrate(ctx, n, outs)
	}
}
