package super

import (
	"context"
	"sync/atomic"
	"testing"

	"nilcore/internal/budget"
	"nilcore/internal/model"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
)

// The MaxFanout rail bounds the concurrently-OUTSTANDING cohort of one
// decomposition wave, not the cumulative count of spawns across the whole run.
// With MaxFanout=2, a third spawn in a wave whose first two are still
// outstanding must be refused (spawn_denied / max_fanout).
func TestSpawnDeniedAboveMaxFanout(t *testing.T) {
	var spawned int32
	spawnFn := func(_ context.Context, spec SubagentSpec) spawn.Result {
		atomic.AddInt32(&spawned, 1)
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	// Three spawns in one wave, MaxFanout=2: the third is refused because two are
	// still outstanding (never awaited/integrated). Then finish converges.
	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "a"})),
		textResp(toolUse("u2", "spawn_subagent", SubagentSpec{ID: "super.t2", Role: roster.RoleImplementer, Goal: "b"})),
		textResp(toolUse("u3", "spawn_subagent", SubagentSpec{ID: "super.t3", Role: roster.RoleImplementer, Goal: "c"})),
		textResp(toolUse("u4", "finish", map[string]string{"summary": "done"})),
	}}
	s := baseSup(m, passVerifier{})
	s.Spawn = spawnFn
	s.MaxFanout = 2

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&spawned); got != 2 {
		t.Fatalf("spawned %d workers, want 2 (MaxFanout must refuse the 3rd)", got)
	}
	if out.Spawned != 2 {
		t.Errorf("Outcome.Spawned = %d, want 2", out.Spawned)
	}
	if !lastResultMentions(m.lastMsgs, "max_fanout") {
		t.Error("a denied spawn must return a max_fanout refusal to the model")
	}
}

// MaxFanout is a PER-WAVE budget, not a cumulative run cap: once a cohort's
// results are folded (here via await_results), a later wave gets the full
// fanout budget back. Before the fix, len(st.handles) only grew, so with
// MaxFanout=2 the third spawn (in a second wave) was wrongly refused even though
// the first two had already been awaited.
func TestMaxFanoutReleasedAfterAwait(t *testing.T) {
	var spawned int32
	spawnFn := func(_ context.Context, spec SubagentSpec) spawn.Result {
		atomic.AddInt32(&spawned, 1)
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	// Wave 1: two spawns (fills the fanout budget). await_results folds both.
	// Wave 2: two more spawns — must be ADMITTED because the prior cohort was folded.
	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "a"})),
		textResp(toolUse("u2", "spawn_subagent", SubagentSpec{ID: "super.t2", Role: roster.RoleImplementer, Goal: "b"})),
		textResp(toolUse("a1", "await_results", map[string]string{})),
		textResp(toolUse("u3", "spawn_subagent", SubagentSpec{ID: "super.t3", Role: roster.RoleImplementer, Goal: "c"})),
		textResp(toolUse("u4", "spawn_subagent", SubagentSpec{ID: "super.t4", Role: roster.RoleImplementer, Goal: "d"})),
		textResp(toolUse("u5", "finish", map[string]string{"summary": "done"})),
	}}
	s := baseSup(m, passVerifier{})
	s.Spawn = spawnFn
	s.MaxFanout = 2

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&spawned); got != 4 {
		t.Fatalf("spawned %d workers, want 4 (fanout budget must reset after await)", got)
	}
	if out.Spawned != 4 {
		t.Errorf("Outcome.Spawned = %d, want 4", out.Spawned)
	}
	if !out.Done {
		t.Error("run should converge Done with a passing verifier")
	}
}

// A merged wave (integrate) also releases the fanout budget: a handle whose
// branch reached the integration tip is folded and stops counting.
func TestMaxFanoutReleasedAfterIntegrate(t *testing.T) {
	var spawned int32
	spawnFn := func(_ context.Context, spec SubagentSpec) spawn.Result {
		atomic.AddInt32(&spawned, 1)
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "a"})),
		textResp(toolUse("u2", "spawn_subagent", SubagentSpec{ID: "super.t2", Role: roster.RoleImplementer, Goal: "b"})),
		textResp(toolUse("i1", "integrate", map[string]string{})),
		textResp(toolUse("u3", "spawn_subagent", SubagentSpec{ID: "super.t3", Role: roster.RoleImplementer, Goal: "c"})),
		textResp(toolUse("u4", "spawn_subagent", SubagentSpec{ID: "super.t4", Role: roster.RoleImplementer, Goal: "d"})),
		textResp(toolUse("u5", "finish", map[string]string{"summary": "done"})),
	}}
	s := baseSup(m, passVerifier{})
	s.Spawn = spawnFn
	s.Integrate = noopIntegrate("integ", nil)
	s.MaxFanout = 2

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&spawned); got != 4 {
		t.Fatalf("spawned %d workers, want 4 (fanout budget must reset after integrate)", got)
	}
	if !out.Done {
		t.Error("run should converge Done with a passing verifier")
	}
}

// A wired Budget ledger is READ (not a count rail): the run's live spend is recorded on
// the terminal super_done event so the audit trail shows the cost. This is the wiring of
// the formerly-dead Budget field.
func TestSupervisorRecordsBudgetSpend(t *testing.T) {
	log, path := openChainLog(t)
	led := budget.New()
	// Simulate a charge the metered provider would have booked during the run.
	if err := led.Charge(context.Background(), "super", 1000, 0.25); err != nil {
		t.Fatalf("charge: %v", err)
	}
	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "finish", map[string]string{"summary": "done"})),
	}}
	s := &Supervisor{Model: m, Verify: passVerifier{}.Check, MaxRounds: 3, MaxDepth: 1, Log: log, Budget: led}
	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events := readLogEvents(t, log, path)
	var found bool
	for _, e := range events {
		if e.Kind != "super_done" {
			continue
		}
		found = true
		if e.Detail["spent_dollars"] == nil || e.Detail["spent_tokens"] == nil {
			t.Fatalf("super_done must carry the wired budget spend, got %+v", e.Detail)
		}
		if d, ok := e.Detail["spent_dollars"].(float64); !ok || d != 0.25 {
			t.Errorf("spent_dollars = %v, want 0.25", e.Detail["spent_dollars"])
		}
	}
	if !found {
		t.Fatal("no super_done event recorded")
	}
}
