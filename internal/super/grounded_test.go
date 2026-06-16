package super

import (
	"context"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/roster"
	"nilcore/internal/spawn"
)

// buildRunContext renders a consistent point-in-time view of runState: cohort states
// derived from each handle's Done/Passed, the verified branch + clipped report for a
// passed node, and the integration tip — sorted by ID for determinism.
func TestBuildRunContextReflectsState(t *testing.T) {
	s := &Supervisor{}
	st := &runState{
		handles:    map[string]*Handle{},
		goal:       "ship the thing",
		planDigest: "t1: a\nt2: b [needs t1]",
		branch:     "integrate/tip",
	}
	st.handles["super.t2"] = &Handle{Spec: SubagentSpec{ID: "super.t2", Role: roster.RoleImplementer},
		Done: true, Result: spawn.Result{Passed: true, Branch: "task/super.t2", Summary: "did\nthe work"}}
	st.handles["super.t1"] = &Handle{Spec: SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer},
		Done: true, Result: spawn.Result{Passed: false}}
	st.handles["super.t3"] = &Handle{Spec: SubagentSpec{ID: "super.t3", Role: roster.RoleResearcher}} // running

	rc := s.buildRunContext(st)
	if rc.Goal != "ship the thing" || rc.Plan != st.planDigest || rc.Tip != "integrate/tip" {
		t.Fatalf("scalars wrong: %+v", rc)
	}
	if len(rc.Cohort) != 3 {
		t.Fatalf("cohort size = %d, want 3", len(rc.Cohort))
	}
	// Sorted by ID: t1, t2, t3.
	if rc.Cohort[0].ID != "super.t1" || rc.Cohort[1].ID != "super.t2" || rc.Cohort[2].ID != "super.t3" {
		t.Errorf("cohort not sorted by ID: %v", rc.Cohort)
	}
	if rc.Cohort[0].State != "failed" {
		t.Errorf("t1 (Passed=false) state = %q, want failed", rc.Cohort[0].State)
	}
	if rc.Cohort[1].State != "passed" || rc.Cohort[1].Branch != "task/super.t2" {
		t.Errorf("t2 = %+v, want passed + branch", rc.Cohort[1])
	}
	if strings.Contains(rc.Cohort[1].Report, "\n") {
		t.Errorf("report should be a one-line clip, got %q", rc.Cohort[1].Report)
	}
	if rc.Cohort[2].State != "running" {
		t.Errorf("t3 (not Done) state = %q, want running", rc.Cohort[2].State)
	}
}

// refreshAndPublish re-points the read tree via RefreshRead (storing its file-tree on
// the snapshot) then publishes; with no RefreshRead wired it is just a publish (no
// tree), and it only refreshes when a tip exists.
func TestRefreshAndPublish(t *testing.T) {
	var gotTip string
	s := &Supervisor{RefreshRead: func(_ context.Context, tip string) string {
		gotTip = tip
		return "server/main.go\nserver/handler.go"
	}}
	st := &runState{handles: map[string]*Handle{}, goal: "g", branch: "integrate/tip-3"}

	s.refreshAndPublish(context.Background(), st)
	if gotTip != "integrate/tip-3" {
		t.Errorf("RefreshRead called with tip %q, want integrate/tip-3", gotTip)
	}
	rc := s.loadRunContext()
	if rc.Tree != "server/main.go\nserver/handler.go" {
		t.Errorf("snapshot Tree = %q, want the RefreshRead file-tree", rc.Tree)
	}

	// No RefreshRead wired: just a publish, no tree, no panic.
	s2 := &Supervisor{}
	st2 := &runState{handles: map[string]*Handle{}, goal: "g", branch: "tip"}
	s2.refreshAndPublish(context.Background(), st2)
	if s2.loadRunContext().Tree != "" {
		t.Error("no RefreshRead wired must leave Tree empty")
	}

	// Empty tip: RefreshRead is NOT called (nothing verified yet).
	called := false
	s3 := &Supervisor{RefreshRead: func(_ context.Context, _ string) string { called = true; return "x" }}
	s3.refreshAndPublish(context.Background(), &runState{handles: map[string]*Handle{}, branch: ""})
	if called {
		t.Error("RefreshRead must not be called with an empty tip")
	}
}

// The publish (main goroutine) / load (reader goroutine) hand-off is race-free: the
// snapshot is a value copy guarded by snapMu, never aliasing live runState. Run under
// -race: concurrent publishes (with a mutating runState) and loads must not race.
func TestRunContextPublishLoadRace(t *testing.T) {
	s := &Supervisor{}
	st := &runState{handles: map[string]*Handle{}, goal: "g"}

	var wg sync.WaitGroup
	wg.Add(2)
	// Producer: the main goroutine mutating runState and re-publishing (as dispatch does).
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			id := "super." + string(rune('a'+(i%8)))
			st.handles[id] = &Handle{Spec: SubagentSpec{ID: id, Role: roster.RoleImplementer},
				Done: true, Result: spawn.Result{Passed: true, Branch: "task/" + id, Summary: "x"}}
			st.branch = "tip-" + id
			s.publishRunContext(s.buildRunContext(st))
		}
	}()
	// Consumer: the reader goroutine loading the snapshot (as answerBody does) and
	// reading every field of the returned copy.
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			rc := s.loadRunContext()
			_ = rc.Goal + rc.Plan + rc.Tip
			for _, c := range rc.Cohort {
				_ = c.ID + c.State + c.Branch + c.Report
			}
		}
	}()
	wg.Wait()
}
