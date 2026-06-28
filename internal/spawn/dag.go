package spawn

// DAGScheduler runs subtasks that depend on one another, releasing a node only
// once ALL of its dependencies have Passed (and, in the supervisor, been merged
// into the integration tip). It is the ordered counterpart to Spawner: Spawner
// fans every task out at once and drops DependsOn; the DAGScheduler honors the
// edges so a consumer (test→implementation, lib→app) is never coded before the
// thing it builds on exists.
//
// WHY a wave-based Kahn release rather than a streaming callback: termination and
// the four terminal states are far easier to prove when each round is a clean
// snapshot. Indegree only decreases, the released set prevents double-enqueue,
// and a round that releases nothing while nodes remain is — by construction —
// a cycle (or all-blocked-by-failure), so we resolve those and stop. There is no
// condition under which the loop spins.
//
// Concurrency within a wave reuses the race-tested scheduler.Scheduler pool, so
// the only concurrency invariant worth trusting (the MaxConcurrent cap) lives in
// one already-proven place. The DAG layer itself touches shared state only
// between waves, single-goroutine, so it adds no new race surface.

import (
	"context"
	"fmt"
	"sync"

	"nilcore/internal/scheduler"
)

// DAGScheduler releases ready subtasks under a concurrency cap and resolves the
// terminal State of every node. The zero value is unusable; set Run.
type DAGScheduler struct {
	// MaxConcurrent caps how many ready nodes run at once within a wave. <1 → 1.
	MaxConcurrent int
	// RunSub executes one released subtask. It must not itself wait on a sibling;
	// dependency ordering is the scheduler's job. Same RunFunc shape the Spawner
	// and the orchestrator's RunSub use. (Named RunSub, not Run, because Run is
	// the scheduler's own entry-point method.)
	RunSub RunFunc
	// OnReady, if set, is called just before a node is released and may rewrite
	// the Subtask — e.g. to point its start-point at the current integration tip
	// (the merged result of its dependencies). It runs single-goroutine between
	// waves, so it may safely read accumulated results.
	OnReady func(st Subtask) Subtask
}

// node is the scheduler's mutable bookkeeping for one subtask. The wave loop
// reads dependency outcomes straight from the results map (a clean per-round
// snapshot), so it needs only the resolved dep list plus two latches.
type node struct {
	st       Subtask
	deps     []string // distinct, existing dependency IDs
	released bool     // has been (or is being) run — prevents re-enqueue
	done     bool     // has a terminal Result
}

// Run schedules subs honoring DependsOn and returns the terminal Result of every
// input node, keyed by ID. Guarantees: every node ends Passed, Failed, Skipped,
// or Cycle; the loop always terminates (indegree-monotone + cycle break); a node
// runs only after all its deps Passed; a failed/skipped/cyclic dep skips its
// dependents transitively. Unknown dependency IDs are ignored (the planner's
// Validate rejects them upstream; here we degrade gracefully rather than hang).
func (d *DAGScheduler) Run(ctx context.Context, subs []Subtask) map[string]Result {
	results := make(map[string]Result, len(subs))
	nodes := make(map[string]*node, len(subs))
	order := make([]string, 0, len(subs)) // stable, for deterministic skip sweeps

	// Index nodes. A duplicate ID keeps the first occurrence (the planner forbids
	// dupes; we stay defensive rather than panic).
	for _, s := range subs {
		if _, ok := nodes[s.ID]; ok {
			continue
		}
		nodes[s.ID] = &node{st: s}
		order = append(order, s.ID)
	}

	// Resolve each node's dependency list once. Self-edges and unknown deps are
	// dropped; duplicates collapse — so a malformed plan degrades to "fewer
	// edges" rather than a hang.
	for _, id := range order {
		n := nodes[id]
		seen := make(map[string]bool)
		for _, dep := range n.st.DependsOn {
			if dep == id || seen[dep] {
				continue // self-dependency or duplicate: ignore
			}
			if _, ok := nodes[dep]; !ok {
				continue // unknown dep: ignore (Validate handles real plans)
			}
			seen[dep] = true
			n.deps = append(n.deps, dep)
		}
	}

	// Wave loop: run every currently-ready node, fold their results, repeat. A done
	// ctx (deadline/shutdown) stops releasing NEW waves immediately — no point cutting
	// fresh worktrees for work that would only be cancelled; the sweep below marks every
	// still-unrun node Skipped(cancelled).
	for ctx.Err() == nil {
		ready := d.collectReady(order, nodes, results)
		if len(ready) == 0 {
			break
		}
		d.runWave(ctx, ready, nodes, results)
	}

	// Anything still without a result terminated without running. Distinguish the
	// two causes so the outcome is honest: if ctx is done, the scheduler SKIPPED the
	// still-queued nodes on cancellation (a deadline/shutdown), so they are Skipped
	// (cancelled) — not cyclic. Otherwise nothing ever drove their indegree to zero,
	// which is (or is downstream of) a dependency cycle. Either way every node
	// terminates in exactly one state and the loop never spins.
	cancelled := ctx.Err()
	for _, id := range order {
		if _, ok := results[id]; !ok {
			if cancelled != nil {
				results[id] = Result{ID: id, State: StateSkipped, Err: cancelled,
					Summary: "skipped: run cancelled"}
			} else {
				results[id] = Result{ID: id, State: StateCycle,
					Summary: "skipped: dependency cycle"}
			}
			nodes[id].done = true
		}
	}
	return results
}

// collectReady returns subtasks whose dependencies are all resolved Passed and
// that have not yet been released. It also resolves any node whose dependency
// failed/was skipped/cyclic as Skipped (so it never becomes "ready"), which lets
// failure propagate transitively across subsequent waves. Returns nil when no
// node can run this round.
func (d *DAGScheduler) collectReady(order []string, nodes map[string]*node, results map[string]Result) []Subtask {
	var ready []Subtask
	for _, id := range order {
		n := nodes[id]
		if n.released || n.done {
			continue
		}
		blocked := false
		allPassed := true
		for _, dep := range n.deps {
			dr, resolved := results[dep]
			if !resolved {
				allPassed = false // dep still pending: wait
				break
			}
			if dr.State != StatePassed {
				blocked = true // dep failed/skipped/cyclic: this node cannot run
				break
			}
		}
		switch {
		case blocked:
			results[id] = Result{ID: id, State: StateSkipped,
				Summary: "skipped: dependency did not pass"}
			n.done = true
		case allPassed:
			st := n.st
			if d.OnReady != nil {
				st = d.OnReady(st) // re-seed start-point off the merged dep tip
			}
			n.st = st
			n.released = true
			ready = append(ready, st)
		}
	}
	return ready
}

// runWave runs the ready subtasks concurrently under the cap and folds each
// terminal Result back into results. Concurrency and the cap are delegated to
// scheduler.Scheduler (race-tested); the fold happens here, single-goroutine,
// after the pool has drained.
func (d *DAGScheduler) runWave(ctx context.Context, ready []Subtask, nodes map[string]*node, results map[string]Result) {
	sch := scheduler.New(d.MaxConcurrent)

	var mu sync.Mutex
	wave := make(map[string]Result, len(ready))

	for i := range ready {
		st := ready[i]
		sch.Submit(scheduler.Task{
			ID: st.ID,
			Run: func(rctx context.Context) error {
				r := d.runOne(rctx, st)
				mu.Lock()
				wave[st.ID] = r
				mu.Unlock()
				return nil // a subtask failure is a result, not a pool error
			},
		})
	}
	sch.Start(ctx)
	_ = sch.Wait()

	for id, r := range wave {
		results[id] = r
		nodes[id].done = true
	}
}

// runOne executes a single released subtask with the same panic isolation the
// flat Spawner provides (one subworker exploding never crashes the run) and
// normalizes the outcome into a terminal State.
func (d *DAGScheduler) runOne(ctx context.Context, st Subtask) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{ID: st.ID, State: StateFailed,
				Err: fmt.Errorf("subworker panicked: %v", r)}
		}
	}()

	r := d.RunSub(ctx, st)
	r.ID = st.ID
	if r.Passed && r.Err == nil {
		r.State = StatePassed
	} else {
		r.State = StateFailed
	}
	return r
}
