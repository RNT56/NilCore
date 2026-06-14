package spawn

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/planner"
	"nilcore/internal/summarize"
)

func TestSpawnAggregatesAndIsolatesFailures(t *testing.T) {
	s := &Spawner{
		MaxConcurrent: 3,
		Run: func(_ context.Context, st Subtask) Result {
			if st.ID == "boom" {
				panic("subworker exploded")
			}
			return Result{ID: st.ID, Summary: st.Goal + " ok", Passed: true}
		},
	}
	subs := []Subtask{
		{ID: "a", Goal: "do a"},
		{ID: "boom", Goal: "fail"},
		{ID: "c", Goal: "do c"},
	}
	res := s.Spawn(context.Background(), subs)
	if len(res) != 3 {
		t.Fatalf("got %d results, want 3", len(res))
	}
	if res[0].ID != "a" || !res[0].Passed {
		t.Errorf("a = %+v", res[0])
	}
	if res[1].Err == nil {
		t.Error("panicking subworker should be isolated with an Err, not crash the run")
	}
	if res[2].ID != "c" || !res[2].Passed {
		t.Errorf("c = %+v (a sibling failure must not affect it)", res[2])
	}
}

func TestSpawnConcurrencyCap(t *testing.T) {
	var inflight, maxSeen int32
	s := &Spawner{
		MaxConcurrent: 2,
		Run: func(_ context.Context, _ Subtask) Result {
			n := atomic.AddInt32(&inflight, 1)
			for {
				m := atomic.LoadInt32(&maxSeen)
				if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			return Result{Passed: true}
		},
	}
	subs := make([]Subtask, 6)
	for i := range subs {
		subs[i] = Subtask{ID: string(rune('a' + i))}
	}
	s.Spawn(context.Background(), subs)
	if maxSeen > 2 {
		t.Errorf("max concurrent = %d, want <= 2", maxSeen)
	}
}

func TestFromPlan(t *testing.T) {
	tree := planner.Tree{Tasks: []planner.PlanTask{
		{ID: "t1", Goal: "write test", Acceptance: "fails first"},
		{ID: "t2", Goal: "implement", Acceptance: "passes"},
	}}
	subs := FromPlan(tree, summarize.ContextSummary{Constraints: []string{"no deps"}})
	if len(subs) != 2 || subs[0].Goal != "write test" {
		t.Fatalf("subs = %+v", subs)
	}
	if len(subs[0].Summary.Constraints) != 1 || subs[0].Summary.Goal != "write test" {
		t.Errorf("seed summary not carried: %+v", subs[0].Summary)
	}
}
