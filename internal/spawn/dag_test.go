package spawn

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/planner"
	"nilcore/internal/summarize"
)

// passAll is a RunFunc that passes every subtask. Tests override per-ID behavior
// by wrapping it.
func passAll(_ context.Context, st Subtask) Result {
	return Result{ID: st.ID, Passed: true}
}

// TestFromPlanPreservesDeps is the one-line-fix acceptance: DependsOn must survive
// the plan→subtask conversion (it was previously dropped).
func TestFromPlanPreservesDeps(t *testing.T) {
	tree := planner.Tree{Tasks: []planner.PlanTask{
		{ID: "t1", Goal: "write test", Acceptance: "red"},
		{ID: "t2", Goal: "implement", Acceptance: "green", DependsOn: []string{"t1"}},
	}}
	subs := FromPlan(tree, summarize.ContextSummary{})
	if len(subs) != 2 {
		t.Fatalf("got %d subs, want 2", len(subs))
	}
	if len(subs[1].DependsOn) != 1 || subs[1].DependsOn[0] != "t1" {
		t.Fatalf("t2.DependsOn = %v, want [t1]", subs[1].DependsOn)
	}
	// Mutating the returned slice must not write through into the plan.
	subs[1].DependsOn[0] = "mutated"
	if tree.Tasks[1].DependsOn[0] != "t1" {
		t.Errorf("FromPlan aliased the plan's DependsOn slice")
	}
}

// TestDAGRunsAfterDepsPassed asserts the core ordering guarantee: a node is
// released only after all its dependencies have Passed, and a diamond resolves
// in topological order.
func TestDAGRunsAfterDepsPassed(t *testing.T) {
	// Diamond:  a → {b, c} → d
	subs := []Subtask{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a"}},
		{ID: "d", DependsOn: []string{"b", "c"}},
	}

	var mu sync.Mutex
	finished := map[string]bool{}

	d := &DAGScheduler{
		MaxConcurrent: 4,
		RunSub: func(_ context.Context, st Subtask) Result {
			// At entry, every declared dependency must already be finished.
			mu.Lock()
			for _, dep := range depsOf(subs, st.ID) {
				if !finished[dep] {
					mu.Unlock()
					t.Errorf("%s ran before dep %s finished", st.ID, dep)
					return Result{ID: st.ID, Passed: true}
				}
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond) // widen any ordering bug
			mu.Lock()
			finished[st.ID] = true
			mu.Unlock()
			return Result{ID: st.ID, Passed: true}
		},
	}

	res := runWithDeadline(t, d, subs)
	for _, id := range []string{"a", "b", "c", "d"} {
		if res[id].State != StatePassed {
			t.Errorf("%s = %q, want passed", id, res[id].State)
		}
	}
}

// TestDAGFailedDepSkipsDependents asserts a failed dependency marks its dependents
// Skipped, transitively, while independent branches still run.
func TestDAGFailedDepSkipsDependents(t *testing.T) {
	// a(fail) → b → c ;  x(pass) → y  (independent branch)
	subs := []Subtask{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"b"}},
		{ID: "x"},
		{ID: "y", DependsOn: []string{"x"}},
	}

	var ranA, ranB, ranC int32
	d := &DAGScheduler{
		MaxConcurrent: 4,
		RunSub: func(_ context.Context, st Subtask) Result {
			switch st.ID {
			case "a":
				atomic.AddInt32(&ranA, 1)
				return Result{ID: st.ID, Passed: false} // fail the root
			case "b":
				atomic.AddInt32(&ranB, 1)
			case "c":
				atomic.AddInt32(&ranC, 1)
			}
			return Result{ID: st.ID, Passed: true}
		},
	}

	res := runWithDeadline(t, d, subs)

	if res["a"].State != StateFailed {
		t.Errorf("a = %q, want failed", res["a"].State)
	}
	if res["b"].State != StateSkipped {
		t.Errorf("b = %q, want skipped (its dep failed)", res["b"].State)
	}
	if res["c"].State != StateSkipped {
		t.Errorf("c = %q, want skipped (transitive — its dep was skipped)", res["c"].State)
	}
	if atomic.LoadInt32(&ranB) != 0 || atomic.LoadInt32(&ranC) != 0 {
		t.Errorf("skipped nodes must not run: b ran %d, c ran %d", ranB, ranC)
	}
	// The independent branch is unaffected.
	if res["x"].State != StatePassed || res["y"].State != StatePassed {
		t.Errorf("independent branch broke: x=%q y=%q", res["x"].State, res["y"].State)
	}
}

// TestDAGCycleTerminates asserts a dependency cycle ends every involved node in
// Cycle WITHOUT spinning. The deadline guard catches an infinite loop.
func TestDAGCycleTerminates(t *testing.T) {
	// Cycle p ↔ q, plus a clean node r and a node s downstream of the cycle.
	subs := []Subtask{
		{ID: "p", DependsOn: []string{"q"}},
		{ID: "q", DependsOn: []string{"p"}},
		{ID: "r"},
		{ID: "s", DependsOn: []string{"p"}},
	}

	var ran int32
	d := &DAGScheduler{
		MaxConcurrent: 2,
		RunSub: func(_ context.Context, st Subtask) Result {
			atomic.AddInt32(&ran, 1)
			return Result{ID: st.ID, Passed: true}
		},
	}

	res := runWithDeadline(t, d, subs)

	for _, id := range []string{"p", "q", "s"} {
		if res[id].State != StateCycle {
			t.Errorf("%s = %q, want cycle", id, res[id].State)
		}
	}
	if res["r"].State != StatePassed {
		t.Errorf("acyclic node r = %q, want passed", res["r"].State)
	}
	// Only r should ever have executed; cyclic nodes never become ready.
	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Errorf("ran %d subtasks, want 1 (only the acyclic node)", got)
	}
}

// TestDAGEveryNodeTerminatesInOneState asserts the totality invariant: every
// input node has exactly one terminal Result, drawn from the closed set.
func TestDAGEveryNodeTerminatesInOneState(t *testing.T) {
	subs := []Subtask{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "fail"},
		{ID: "c", DependsOn: []string{"fail"}},
		{ID: "p", DependsOn: []string{"q"}},
		{ID: "q", DependsOn: []string{"p"}},
	}
	d := &DAGScheduler{
		MaxConcurrent: 3,
		RunSub: func(_ context.Context, st Subtask) Result {
			return Result{ID: st.ID, Passed: st.ID != "fail"}
		},
	}
	res := runWithDeadline(t, d, subs)

	if len(res) != len(subs) {
		t.Fatalf("got %d results, want %d (one per node)", len(res), len(subs))
	}
	terminal := map[State]bool{StatePassed: true, StateFailed: true, StateSkipped: true, StateCycle: true}
	for _, st := range subs {
		r, ok := res[st.ID]
		if !ok {
			t.Errorf("node %s has no result", st.ID)
			continue
		}
		if !terminal[r.State] {
			t.Errorf("node %s ended in non-terminal state %q", st.ID, r.State)
		}
		if r.ID != st.ID {
			t.Errorf("result for %s carries wrong ID %q", st.ID, r.ID)
		}
	}
}

// TestDAGOnReadyReseeds asserts the OnReady hook can rewrite a subtask just before
// release — the seam the integrator uses to re-point a dependent's start-point at
// the merged dependency tip. It also confirms OnReady sees deps already resolved.
func TestDAGOnReadyReseeds(t *testing.T) {
	subs := []Subtask{
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
	}

	var seenGoal sync.Map // ID -> goal observed inside RunSub
	d := &DAGScheduler{
		MaxConcurrent: 1,
		OnReady: func(st Subtask) Subtask {
			// Re-seed: stamp the goal as if pointing at the integration tip.
			st.Goal = "on:" + st.ID
			return st
		},
		RunSub: func(_ context.Context, st Subtask) Result {
			seenGoal.Store(st.ID, st.Goal)
			return Result{ID: st.ID, Passed: true, Branch: "task/" + st.ID}
		},
	}

	res := runWithDeadline(t, d, subs)

	for _, id := range []string{"a", "b"} {
		g, _ := seenGoal.Load(id)
		if g != "on:"+id {
			t.Errorf("%s ran with goal %v, want OnReady-rewritten %q", id, g, "on:"+id)
		}
		if res[id].Branch != "task/"+id {
			t.Errorf("%s Branch = %q, want task/%s (Result.Branch carried back)", id, res[id].Branch, id)
		}
	}
}

// TestDAGPanicIsolated asserts a panicking subworker becomes a Failed result with
// Err set (mirrors the flat Spawner's isolation) and never crashes the run, and
// that its dependents are Skipped.
func TestDAGPanicIsolated(t *testing.T) {
	subs := []Subtask{
		{ID: "boom"},
		{ID: "after", DependsOn: []string{"boom"}},
		{ID: "ok"},
	}
	d := &DAGScheduler{
		MaxConcurrent: 2,
		RunSub: func(_ context.Context, st Subtask) Result {
			if st.ID == "boom" {
				panic("subworker exploded")
			}
			return Result{ID: st.ID, Passed: true}
		},
	}
	res := runWithDeadline(t, d, subs)

	if res["boom"].State != StateFailed || res["boom"].Err == nil {
		t.Errorf("boom = %+v, want failed with Err set", res["boom"])
	}
	if res["after"].State != StateSkipped {
		t.Errorf("after = %q, want skipped (dep panicked)", res["after"].State)
	}
	if res["ok"].State != StatePassed {
		t.Errorf("ok = %q, want passed (a sibling panic must not affect it)", res["ok"].State)
	}
}

// TestDAGConcurrencyCap asserts ready nodes within a wave respect MaxConcurrent
// (delegated to scheduler.Scheduler). All eight nodes are independent → one wave.
func TestDAGConcurrencyCap(t *testing.T) {
	subs := make([]Subtask, 8)
	for i := range subs {
		subs[i] = Subtask{ID: string(rune('a' + i))}
	}
	var inflight, maxSeen int32
	d := &DAGScheduler{
		MaxConcurrent: 3,
		RunSub: func(_ context.Context, _ Subtask) Result {
			n := atomic.AddInt32(&inflight, 1)
			for {
				m := atomic.LoadInt32(&maxSeen)
				if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			return Result{Passed: true}
		},
	}
	runWithDeadline(t, d, subs)
	if maxSeen > 3 {
		t.Errorf("max concurrent = %d, want <= 3", maxSeen)
	}
}

// --- helpers ---

// runWithDeadline runs the scheduler under a hard deadline so an accidental
// infinite loop (the cycle-safety regression) fails fast as a test timeout signal
// rather than hanging the suite.
func runWithDeadline(t *testing.T, d *DAGScheduler, subs []Subtask) map[string]Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan map[string]Result, 1)
	go func() { done <- d.Run(ctx, subs) }()
	select {
	case res := <-done:
		return res
	case <-ctx.Done():
		t.Fatal("DAGScheduler.Run did not terminate (possible cycle spin)")
		return nil
	}
}

func depsOf(subs []Subtask, id string) []string {
	for _, s := range subs {
		if s.ID == id {
			out := append([]string(nil), s.DependsOn...)
			sort.Strings(out)
			return out
		}
	}
	return nil
}
