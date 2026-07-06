package agent_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/trust"
)

// RTE-T05 — wiring the nil-safe TrustOracle into the orchestrator's candidate-build,
// race-N, and escalate paths. These tests exercise the integration at the
// orchestrator boundary (the oracle.go helpers are unit-tested in oracle_test.go).
//
// fakeOracle (defined in oracle_test.go, same package) REVERSES the candidate order
// and carries fixed sizing hints — enough to prove the orchestrator consumes the
// oracle's plan and sizing without any trust/store dependency.

// findEscalate returns the first race_escalate event in the log, or fails.
func findEscalate(t *testing.T, path string) logEvent {
	t.Helper()
	for _, e := range readEvents(t, path) {
		if e.Kind == "race_escalate" {
			return e
		}
	}
	t.Fatal("no race_escalate event in log")
	return logEvent{}
}

// TestOracleNilRaceEscalateByteIdentical is the golden DEFAULT-OFF proof: with a nil
// Oracle (and a nil Cost) the single-path race_escalate event carries ONLY the legacy
// {"n": RaceN} shape — no class, no cost — exactly as before this seam. Nothing the
// oracle wiring added shows up on the default path.
func TestOracleNilRaceEscalateByteIdentical(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	var calls int
	newEnv := func(string) agent.Env {
		calls++
		// Single attempt fails; the race copies pass (existing RaceN escalation).
		return agent.Env{Backend: &fakeBackend{name: "solo"}, Verifier: &fakeVerifier{passed: calls > 1}}
	}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		NewEnv:   newEnv,
		RaceN:    2,
		// Oracle and Cost both nil ⇒ default-off, byte-identical.
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "nil-oracle", Goal: "refactor the thing"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()
	if !out.Verified || out.Backend != "solo" {
		t.Fatalf("default-off escalation: got %+v, want verified solo", out)
	}

	e := findEscalate(t, logPath)
	if n, _ := e.Detail["n"].(float64); int(n) != 2 {
		t.Errorf("race_escalate n = %v, want 2 (static RaceN)", e.Detail["n"])
	}
	if _, has := e.Detail["class"]; has {
		t.Error("nil Oracle must NOT stamp class on race_escalate (byte-identical)")
	}
	if _, has := e.Detail["cost"]; has {
		t.Error("nil Oracle/Cost must NOT stamp cost on race_escalate (byte-identical)")
	}
	// And no backend_select event (that is the multi path) on the single path.
	for _, ev := range readEvents(t, logPath) {
		if ev.Kind == "backend_select" {
			t.Error("single path must NOT emit backend_select")
		}
	}
}

// TestOracleReordersMultiBackendCandidates proves a wired Oracle reorders the
// candidate set the orchestrator runs: with configured [a,b,c], no Selector, and a
// reversing oracle, the single-task pick is c (the reversed-first) and the
// backend_select event records the reversed order with by="trust".
func TestOracleReordersMultiBackendCandidates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	built := map[string]*fakeBackend{}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		Backends: []string{"a", "b", "c"},
		NewEnvFor: func(_, name string) agent.Env {
			fb := &fakeBackend{name: name}
			built[name] = fb
			return agent.Env{Backend: fb, Verifier: &fakeVerifier{passed: true}}
		},
		Oracle: fakeOracle{}, // reverses [a,b,c] ⇒ [c,b,a]
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "reorder", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if out.Backend != "c" {
		t.Errorf("ran backend %q, want c (oracle reversed [a,b,c] ⇒ c first)", out.Backend)
	}
	if built["a"] != nil && built["a"].ran || built["b"] != nil && built["b"].ran {
		t.Error("only the oracle's first-ordered backend should run on a passing single-task")
	}

	var sawSelect bool
	for _, e := range readEvents(t, logPath) {
		if e.Kind != "backend_select" {
			continue
		}
		sawSelect = true
		if e.Detail["chosen"] != "c" {
			t.Errorf("backend_select chose %v, want c", e.Detail["chosen"])
		}
		if order := toStrings(e.Detail["order"]); !reflect.DeepEqual(order, []string{"c", "b", "a"}) {
			t.Errorf("backend_select order = %v, want [c b a] (oracle reversed)", order)
		}
		if e.Detail["by"] != "trust" {
			t.Errorf("backend_select by = %v, want trust (the Oracle ordered them)", e.Detail["by"])
		}
	}
	if !sawSelect {
		t.Error("expected a backend_select event when the Oracle is wired")
	}
}

// TestCostAwareOracleReordersRunOrder is the end-to-end proof of the cost-aware
// routing wiring (RTE-T06): a REAL TrustRouteOracle built with a non-nil CostFunc,
// over a warm ledger where both configured backends have equally cleared the
// confidence bar, makes the orchestrator run the CHEAPER backend first — and a
// COST-BLIND oracle over the identical ledger runs the other one (name order). Same
// ledger, same candidates: the ONLY difference is whether a cost func was supplied,
// so a change in which backend runs proves the supplied cost func changed routing
// order (previously dead because the composition passed a nil cost).
func TestCostAwareOracleReordersRunOrder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Warm ledger: "aexpensive" and "zcheap" both 9/10 on the "feature" class, so both
	// clear the confidence bar with an identical smoothed rate — the tie the cost
	// dimension breaks. Names chosen so cost-blind (name order) puts aexpensive first.
	newLedger := func() *trust.Ledger {
		l := trust.New()
		for i := 0; i < 10; i++ {
			l.Record(trust.Outcome{Backend: "aexpensive", Class: "feature", Passed: i < 9})
			l.Record(trust.Outcome{Backend: "zcheap", Class: "feature", Passed: i < 9})
		}
		return l
	}
	// zcheap is cheaper (via a real PricerCost over a stub pricer + model-id resolver).
	pricer := stubPricer{rate: map[string]float64{"expensive-model": 1.00, "cheap-model": 0.10}}
	modelID := func(backend string) string {
		switch backend {
		case "aexpensive":
			return "expensive-model"
		case "zcheap":
			return "cheap-model"
		default:
			return ""
		}
	}
	cost := agent.PricerCost(pricer, modelID, 1000, 1000)

	run := func(t *testing.T, oracle agent.TrustOracle) string {
		t.Helper()
		repo := initGitRepo(t)
		logPath := filepath.Join(t.TempDir(), "events.log")
		lg, err := eventlog.Open(logPath)
		if err != nil {
			t.Fatalf("eventlog.Open: %v", err)
		}
		defer lg.Close()
		orch := &agent.Orchestrator{
			BaseRepo: repo,
			Log:      lg,
			// "add a feature" classifies as the "feature" class the ledger is warm on.
			Backends: []string{"aexpensive", "zcheap"},
			NewEnvFor: func(_, name string) agent.Env {
				return agent.Env{Backend: &fakeBackend{name: name}, Verifier: &fakeVerifier{passed: true}}
			},
			Oracle: oracle,
		}
		out, err := orch.Execute(context.Background(), backend.Task{ID: "cost", Goal: "add a feature"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out.Backend
	}

	// Cost-aware: the cheaper backend runs first.
	if got := run(t, agent.NewTrustRouteOracle(newLedger(), cost)); got != "zcheap" {
		t.Errorf("cost-aware oracle ran %q first, want zcheap (cheapest of the cleared tier)", got)
	}
	// Cost-blind over the SAME ledger: the tie falls to name order ⇒ aexpensive first.
	// Proves the ONLY thing that flipped the order was supplying a cost func.
	if got := run(t, agent.NewTrustRouteOracle(newLedger(), nil)); got != "aexpensive" {
		t.Errorf("cost-blind oracle ran %q first, want aexpensive (name-order tie-break)", got)
	}
}

// TestOracleStampsClassAndCostOnRace proves the routing LEARNING dimensions ride the
// race_escalate event when routing/cost is wired: the task class is recorded, and a
// wired Cost func records a per-candidate cost map — the metadata trust.Replay folds
// the per-(class, backend) cell from. The verifier still judges the race (I2): a fails,
// b passes, b wins.
func TestOracleStampsClassAndCostOnRace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	costs := map[string]float64{"a": 0.50, "b": 0.10}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		Backends: []string{"a", "b"},
		// The oracle reverses [a,b] ⇒ [b,a], so the single pick is b. Make the verifier
		// pass ONLY a: the single pick (b) fails → escalate to a race → a clears it. The
		// verifier picks the winner (a), not the oracle (which ordered b first).
		NewEnvFor: func(_, name string) agent.Env {
			return agent.Env{Backend: &fakeBackend{name: name}, Verifier: &fakeVerifier{passed: name == "a"}}
		},
		Oracle: fakeOracle{},
		Cost:   func(_ /*class*/, backendName string) float64 { return costs[backendName] },
	}

	// Goal classifies as "refactor" (keyword) so the recorded class is deterministic.
	out, err := orch.Execute(context.Background(), backend.Task{ID: "class-cost", Goal: "refactor the parser"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if !out.Verified || out.Backend != "a" {
		t.Fatalf("race winner = %+v, want verified a (verifier judges, not the oracle)", out)
	}

	e := findEscalate(t, logPath)
	if e.Detail["class"] != "refactor" {
		t.Errorf("race_escalate class = %v, want refactor", e.Detail["class"])
	}
	cost, ok := e.Detail["cost"].(map[string]any)
	if !ok {
		t.Fatalf("race_escalate cost = %v (%T), want a per-backend map", e.Detail["cost"], e.Detail["cost"])
	}
	if got, _ := cost["a"].(float64); got != 0.50 {
		t.Errorf("cost[a] = %v, want 0.50", cost["a"])
	}
	if got, _ := cost["b"].(float64); got != 0.10 {
		t.Errorf("cost[b] = %v, want 0.10", cost["b"])
	}
}

// TestOracleSizesSinglePathRaceN proves the oracle SIZES the best-of-N: on the SINGLE
// path (one backend, static RaceN left at 0 — which alone would never race), an oracle
// that returns RaceN=2 for the class makes the escalation fire and recover. The
// verifier still judges the race (I2).
func TestOracleSizesSinglePathRaceN(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	var calls int
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		NewEnv: func(string) agent.Env {
			calls++
			// Single attempt fails; the oracle-sized race copies pass.
			return agent.Env{Backend: &fakeBackend{name: "solo"}, Verifier: &fakeVerifier{passed: calls > 1}}
		},
		// RaceN stays 0 (static gate would NOT race); the oracle sizes it to 2.
		Oracle: fakeOracle{raceN: 2},
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "sized", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if !out.Verified || out.Backend != "solo" {
		t.Fatalf("oracle-sized single-path race: got %+v, want verified solo", out)
	}
	e := findEscalate(t, logPath)
	if n, _ := e.Detail["n"].(float64); int(n) != 2 {
		t.Errorf("race_escalate n = %v, want 2 (oracle-sized RaceN)", e.Detail["n"])
	}
	// The oracle is wired ⇒ the class rides the event even on the single path.
	if e.Detail["class"] == nil {
		t.Error("a wired Oracle should stamp class on the single-path race_escalate")
	}
}
