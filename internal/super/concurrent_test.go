package super

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/agent/bus"
	"nilcore/internal/model"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
	"nilcore/internal/strongcap"
)

// spawnWave builds a one-turn model script that spawns the given specs, then
// await_results, integrate, and finish — the canonical decomposition a supervisor
// drives. Reused across the concurrency property tests.
func spawnWave(specs ...SubagentSpec) *scriptModel {
	turn := make([]model.Block, 0, len(specs))
	for i, sp := range specs {
		turn = append(turn, toolUse(fmt.Sprintf("s%d", i), "spawn_subagent", sp))
	}
	return &scriptModel{responses: []model.Response{
		textResp(turn...),
		textResp(toolUse("await", "await_results", map[string]any{})),
		textResp(toolUse("intg", "integrate", map[string]any{})),
		textResp(toolUse("fin", "finish", map[string]string{"summary": "done"})),
	}}
}

func implSpec(id string, deps ...string) SubagentSpec {
	return SubagentSpec{ID: id, Role: roster.RoleImplementer, Goal: "work " + id, DependsOn: deps}
}

// --- 1. A wave of independent subagents converges; all run concurrently ----------

func TestConcurrentWaveConverges(t *testing.T) {
	var ran int32
	m := spawnWave(implSpec("super.t1"), implSpec("super.t2"), implSpec("super.t3"), implSpec("super.t4"))
	s := baseSup(m, passVerifier{})
	s.Concurrency = 4
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		atomic.AddInt32(&ran, 1)
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	var order []string
	s.Integrate = noopIntegrate("integrate/tip", &order)

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != "converged" {
		t.Fatalf("want converged, got Done=%t reason=%q", out.Done, out.Reason)
	}
	if ran != 4 {
		t.Errorf("all 4 subagents should have run, got %d", ran)
	}
	if out.Spawned != 4 {
		t.Errorf("Outcome.Spawned = %d, want 4", out.Spawned)
	}
	if len(order) != 4 {
		t.Errorf("all 4 passing branches should integrate, got %d", len(order))
	}
}

// --- 2. Peak concurrency never exceeds the cap -----------------------------------

func TestConcurrentPeakWithinCap(t *testing.T) {
	const cap = 2
	var inFlight, peak int32
	m := spawnWave(implSpec("super.t1"), implSpec("super.t2"), implSpec("super.t3"),
		implSpec("super.t4"), implSpec("super.t5"), implSpec("super.t6"))
	s := baseSup(m, passVerifier{})
	s.Concurrency = cap
	// Each worker holds its slot briefly so overlap is observable; the high-water
	// mark of concurrent invocations must never exceed the cap.
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	s.Integrate = noopIntegrate("integrate/tip", nil)

	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peak > cap {
		t.Errorf("peak concurrent workers = %d, exceeds cap %d", peak, cap)
	}
	if peak < 2 {
		t.Errorf("peak = %d: the wave did not actually run concurrently", peak)
	}
}

// --- 3. A failed node skips its dependents; the red combo never poisons the tip --

func TestConcurrentFailedNodeSkipsDependents(t *testing.T) {
	var t2ran int32
	// t1 fails; t2 depends_on t1 (must be Skipped, never run); t3 is independent and
	// passes. Only t3's branch reaches the integrator — the maximal green prefix.
	m := spawnWave(implSpec("super.t1"), implSpec("super.t2", "super.t1"), implSpec("super.t3"))
	s := baseSup(m, passVerifier{})
	s.Concurrency = 3
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		switch spec.ID {
		case "super.t1":
			return spawn.Result{ID: spec.ID, Passed: false} // red — no branch
		case "super.t2":
			atomic.AddInt32(&t2ran, 1)
			return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
		default:
			return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
		}
	}
	var order []string
	s.Integrate = noopIntegrate("integrate/tip", &order)

	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if t2ran != 0 {
		t.Errorf("super.t2 depends on the failed super.t1 and must NOT run, ran %d times", t2ran)
	}
	// Only super.t3 (passed, independent) is integrated; t1 (red) and t2 (skipped) are not.
	if len(order) != 1 || order[0] != "super.t3" {
		t.Errorf("integration order = %v, want only [super.t3] (red combo never poisons the tip)", order)
	}
}

// --- 4. No deadlock: a worker blocking on ask_supervisor AND ask_advisor resolves -

func TestConcurrentNoDeadlockAskSupervisorAndAdvisor(t *testing.T) {
	b := bus.New(nil, 8, 0)
	const answer = "stdlib only, no deps"

	// The advisor tier is a cap-1 strongcap over a fake strong model, so the worker
	// herd CONTENDS for the single advisor slot — the limiter must serialize them
	// without hanging (ctx-bounded), while ask_supervisor stays independent.
	advProv := strongcap.New(&immediateModel{}, 1)

	m := spawnWave(implSpec("super.t1"), implSpec("super.t2"), implSpec("super.t3"))
	s := baseSup(m, passVerifier{})
	s.Concurrency = 3
	s.Bus = b
	s.Answer = func(_ context.Context, _ bus.Message, _ RunContext) string { return answer }
	s.Spawn = func(ctx context.Context, spec SubagentSpec) spawn.Result {
		peer, err := bus.NewPeer(b, bus.AgentID(spec.ID))
		if err != nil {
			return spawn.Result{ID: spec.ID, Err: err}
		}
		_ = peer
		defer b.Deregister(bus.AgentID(spec.ID))

		// (a) ask_supervisor — a real blocking bus.Ask, answered by the dedicated reader.
		askCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		reply, aerr := b.Ask(askCtx, bus.Message{
			Sender: spec.ID, To: []bus.AgentID{bus.Supervisor},
			Kind: bus.KindQuestion, Payload: "which lib?", TTL: 8,
		})
		if aerr != nil {
			return spawn.Result{ID: spec.ID, Err: fmt.Errorf("ask_supervisor: %w", aerr)}
		}
		// (b) ask_advisor — through the cap-1 limiter; all three contend, none hangs.
		if _, cerr := advProv.Complete(ctx, "sys", nil, nil, 256); cerr != nil {
			return spawn.Result{ID: spec.ID, Err: fmt.Errorf("ask_advisor: %w", cerr)}
		}
		if reply.Payload != answer {
			return spawn.Result{ID: spec.ID, Passed: false}
		}
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	s.Integrate = noopIntegrate("integrate/tip", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan Outcome, 1)
	go func() {
		out, err := s.Run(ctx, "goal")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- out
	}()
	select {
	case out := <-done:
		if !out.Done {
			t.Fatalf("run did not converge — a worker's escalation did not resolve (Done=%t)", out.Done)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: a concurrent wave hung on ask_supervisor / ask_advisor")
	}
}

// --- 5. Cancellation mid-wave drains; nothing hangs ------------------------------

func TestConcurrentCancellationDrains(t *testing.T) {
	started := make(chan struct{}, 4)
	s := baseSup(&scriptModel{}, passVerifier{})
	s.Concurrency = 4
	s.Spawn = func(ctx context.Context, spec SubagentSpec) spawn.Result {
		started <- struct{}{}
		<-ctx.Done() // simulate long work cut short by cancellation
		return spawn.Result{ID: spec.ID, Passed: false, State: spawn.StateFailed, Err: ctx.Err()}
	}

	ctx, cancel := context.WithCancel(context.Background())
	batch := []model.Block{
		toolUse("s0", "spawn_subagent", implSpec("super.t1")),
		toolUse("s1", "spawn_subagent", implSpec("super.t2")),
		toolUse("s2", "spawn_subagent", implSpec("super.t3")),
		toolUse("s3", "spawn_subagent", implSpec("super.t4")),
	}
	st := &runState{handles: map[string]*Handle{}}

	resCh := make(chan []model.Block, 1)
	go func() { resCh <- s.runSpawnWave(ctx, 0, st, batch) }()

	for i := 0; i < 4; i++ { // all four workers are in-flight
		<-started
	}
	cancel() // pull the rug

	select {
	case res := <-resCh:
		if len(res) != 4 {
			t.Fatalf("every block must get a terminal tool_result, got %d", len(res))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runSpawnWave hung after ctx cancellation — Wait did not drain")
	}
}

// --- 6. Serial and concurrent produce the same outcome ---------------------------

func TestSerialConcurrentSameOutcome(t *testing.T) {
	build := func(conc int) (Outcome, []string) {
		m := spawnWave(implSpec("super.t1"), implSpec("super.t2", "super.t1"), implSpec("super.t3"))
		s := baseSup(m, passVerifier{})
		s.Concurrency = conc
		s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
			return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
		}
		var order []string
		s.Integrate = noopIntegrate("integrate/tip", &order)
		out, err := s.Run(context.Background(), "goal")
		if err != nil {
			t.Fatalf("Run(conc=%d): %v", conc, err)
		}
		return out, order
	}
	serial, sOrder := build(1)
	conc, cOrder := build(4)

	if serial.Done != conc.Done || serial.Reason != conc.Reason || serial.Branch != conc.Branch || serial.Spawned != conc.Spawned {
		t.Errorf("outcome diverged:\n serial=%+v\n  conc=%+v", serial, conc)
	}
	// A dependency must precede its dependent in BOTH (mergeOrder is dependency-stable).
	if pos(sOrder, "super.t1") > pos(sOrder, "super.t2") {
		t.Errorf("serial order broke the dep edge: %v", sOrder)
	}
	if pos(cOrder, "super.t1") > pos(cOrder, "super.t2") {
		t.Errorf("concurrent order broke the dep edge: %v", cOrder)
	}
	if len(sOrder) != len(cOrder) {
		t.Errorf("merge set size diverged: serial=%v concurrent=%v", sOrder, cOrder)
	}
}

// --- 7. Pre-wave validation rejects a duplicate id before dispatch ---------------

func TestConcurrentPreWaveRejectsDuplicateID(t *testing.T) {
	var ran int32
	// Two spawn blocks, same id, in one turn at concurrency 2: the second must be
	// rejected by the single-goroutine pre-wave validation — only ONE worker runs,
	// so two workers can never collide on task/<id>.
	m := &scriptModel{responses: []model.Response{
		textResp(
			toolUse("s0", "spawn_subagent", implSpec("super.dup")),
			toolUse("s1", "spawn_subagent", implSpec("super.dup")),
		),
		textResp(toolUse("fin", "finish", map[string]string{"summary": "done"})),
	}}
	s := baseSup(m, passVerifier{})
	s.Concurrency = 2
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		atomic.AddInt32(&ran, 1)
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	s.Integrate = noopIntegrate("integrate/tip", nil)

	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran != 1 {
		t.Errorf("duplicate id must run exactly one worker, ran %d", ran)
	}
	if !lastResultMentions(m.lastMsgs, "already spawned") {
		t.Error("the duplicate-id block should return an 'already spawned' error to the model")
	}
}

// --- 8. runState single-owner under -race (the keystone) -------------------------

// A wide concurrent wave where every worker writes typed fields the supervisor then
// folds. Run under `go test -race`: the detector is the assertion that no worker
// ever touches runState (only the supervisor folds, single-goroutine between waves).
func TestConcurrentRunStateSingleOwnerRace(t *testing.T) {
	const n = 8
	specs := make([]SubagentSpec, n)
	for i := range specs {
		specs[i] = implSpec(fmt.Sprintf("super.t%d", i))
	}
	m := spawnWave(specs...)
	s := baseSup(m, passVerifier{})
	s.Concurrency = n
	var wg sync.WaitGroup
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		wg.Add(1)
		defer wg.Done()
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID,
			Summary: strings.Repeat("x", 64)}
	}
	s.Integrate = noopIntegrate("integrate/tip", nil)

	out, err := s.Run(context.Background(), "goal")
	wg.Wait()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Spawned != n {
		t.Errorf("Spawned = %d, want %d", out.Spawned, n)
	}
}

// --- 9. Multi-dep re-base handoff: a dependent's SpawnFunc gets all dep branches --

// A 3-dep node (super.t4 depends_on t1,t2,t3) must receive BaseRefs = the verified
// branches of ALL THREE passed deps (the Phase-2 multi-dep handoff resolveBaseRefs
// builds); a single-dep node still gets BaseRef and empty BaseRefs. Tested under
// concurrency so the intra-wave `branches` resolution path is exercised.
func TestConcurrentMultiDepBaseRefsHandoff(t *testing.T) {
	var mu sync.Mutex
	gotRefs := map[string][]string{}
	gotBase := map[string]string{}
	m := spawnWave(implSpec("super.t1"), implSpec("super.t2"), implSpec("super.t3"),
		implSpec("super.t4", "super.t1", "super.t2", "super.t3"),
		implSpec("super.t5", "super.t1"))
	s := baseSup(m, passVerifier{})
	s.Concurrency = 4
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		mu.Lock()
		gotRefs[spec.ID] = append([]string(nil), spec.BaseRefs...)
		gotBase[spec.ID] = spec.BaseRef
		mu.Unlock()
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	s.Integrate = noopIntegrate("integrate/tip", nil)

	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The 3-dep node gets all three verified dep branches, in DependsOn order.
	want := []string{"task/super.t1", "task/super.t2", "task/super.t3"}
	if got := gotRefs["super.t4"]; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("super.t4 BaseRefs = %v, want %v", got, want)
	}
	// The single-dep node uses BaseRef (not BaseRefs).
	if len(gotRefs["super.t5"]) != 0 {
		t.Errorf("single-dep super.t5 should have empty BaseRefs, got %v", gotRefs["super.t5"])
	}
	if gotBase["super.t5"] != "task/super.t1" {
		t.Errorf("single-dep super.t5 BaseRef = %q, want task/super.t1", gotBase["super.t5"])
	}
	// Independent nodes get neither.
	if len(gotRefs["super.t1"]) != 0 || gotBase["super.t1"] != "" {
		t.Errorf("independent super.t1 should have no re-base hint, got BaseRefs=%v BaseRef=%q", gotRefs["super.t1"], gotBase["super.t1"])
	}
}

// immediateModel is a no-op strong model for the advisor path: it returns at once so
// the limiter (not the model) is the only thing that can gate the herd.
type immediateModel struct{}

func (immediateModel) Model() string { return "fake-advisor" }
func (immediateModel) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: "advice"}}, StopReason: "end_turn"}, nil
}

func pos(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
