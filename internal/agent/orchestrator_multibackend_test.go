package agent_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/verify"
)

// rankSelector is a fixed-order fake Selector: it sorts the candidate names into
// the configured order, dropping any it does not name (it only ORDERS — the
// verifier still judges, I2). It records that it was consulted.
type rankSelector struct {
	order  []string // best-first
	called bool
}

func (s *rankSelector) Select(_ context.Context, _ backend.Task, names []string) []string {
	s.called = true
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	var out []string
	for _, n := range s.order {
		if have[n] {
			out = append(out, n)
		}
	}
	return out
}

// logEvent is the subset of an on-disk eventlog.Event a test needs to assert
// against: the kind, the backend it names, and its detail payload.
type logEvent struct {
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// readEvents reads back the JSONL event log at path and returns its events. The log
// must be Closed (flushed) before calling.
func readEvents(t *testing.T, path string) []logEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var evs []logEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e logEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("parse log line: %v", err)
		}
		evs = append(evs, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return evs
}

// MULTI single-task: with Backends=[a,b,c], a NewEnvFor that builds a distinct fake
// backend per name, and a Selector that ranks c > a > b, executeSingle runs backend
// c FIRST and logs a backend_select event naming c and the order.
func TestMultiBackendSelectsStrongestFirst(t *testing.T) {
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
	sel := &rankSelector{order: []string{"c", "a", "b"}}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		Backends: []string{"a", "b", "c"},
		NewEnvFor: func(_, name string) agent.Env {
			fb := &fakeBackend{name: name}
			built[name] = fb
			return agent.Env{Backend: fb, Verifier: &fakeVerifier{passed: true}}
		},
		Selector: sel,
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "multi-1", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if out.Backend != "c" {
		t.Errorf("ran backend %q, want c (strongest, tried first)", out.Backend)
	}
	if !sel.called {
		t.Error("the Selector was not consulted on the multi path")
	}
	if built["c"] == nil || !built["c"].ran {
		t.Error("backend c did not run")
	}
	if (built["a"] != nil && built["a"].ran) || (built["b"] != nil && built["b"].ran) {
		t.Error("only the first-ordered backend should run on a passing single-task")
	}

	// The backend_select event names c and carries the ordered candidate list.
	var sawSelect bool
	for _, e := range readEvents(t, logPath) {
		if e.Kind != "backend_select" {
			continue
		}
		sawSelect = true
		if e.Backend != "c" || e.Detail["chosen"] != "c" {
			t.Errorf("backend_select chose %q (detail %v), want c", e.Backend, e.Detail["chosen"])
		}
		order := toStrings(e.Detail["order"])
		if !reflect.DeepEqual(order, []string{"c", "a", "b"}) {
			t.Errorf("backend_select order = %v, want [c a b]", order)
		}
		if e.Detail["by"] != "trust" {
			t.Errorf("backend_select by = %v, want trust (a Selector ordered them)", e.Detail["by"])
		}
	}
	if !sawSelect {
		t.Error("expected a backend_select event on the multi path")
	}
}

// ROBUSTNESS — the empty-Selector guard: a Selector is documented as ordering AND
// FILTERING, so a conforming one may return fewer or (pathologically) ZERO names.
// executeSingle indexes names[0], so orderBackends must never hand back an empty slice —
// it falls back to the configured set. Assert no panic/abort and that the first CONFIGURED
// backend runs.
func TestMultiBackendEmptySelectorFallsBackToConfigured(t *testing.T) {
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
		Backends: []string{"a", "b"},
		NewEnvFor: func(_, name string) agent.Env {
			fb := &fakeBackend{name: name}
			built[name] = fb
			return agent.Env{Backend: fb, Verifier: &fakeVerifier{passed: true}}
		},
		Selector: &rankSelector{order: nil}, // drops every name
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "multi-empty", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute (empty Selector must not panic/abort): %v", err)
	}
	_ = lg.Close()
	if out.Backend != "a" {
		t.Errorf("ran backend %q, want a (first configured) on the empty-Selector fallback", out.Backend)
	}
}

// MULTI race: with two DISTINCT backends where the verifier passes ONLY the
// second-ordered one, raceEscalate races BOTH and returns the verifier-passing one
// — proving the verifier judges the winner, not the Selector. The per-candidate
// race_outcome events carry the two distinct backends (the Trust Ledger signal).
func TestMultiBackendRaceVerifierPicksWinner(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	// Selector orders [a, b] (a is "stronger"), but the verifier passes ONLY b.
	sel := &rankSelector{order: []string{"a", "b"}}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		Backends: []string{"a", "b"},
		NewEnvFor: func(_, name string) agent.Env {
			// a is the first single-task pick: its verifier FAILS (forces escalation).
			// In the race, a still fails and b passes — the verifier picks b.
			return agent.Env{Backend: &fakeBackend{name: name}, Verifier: &fakeVerifier{passed: name == "b"}}
		},
		Selector: sel,
		// RaceN left at the zero value (1 ⇒ no single-path race): the multi path
		// escalates on multiBackend() alone, so the cross-backend race fires WITHOUT
		// the operator also setting RaceN — proven by recovery below.
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "race-1", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if !out.Verified {
		t.Fatal("the race had a verifier-passing backend (b); expected Verified")
	}
	if out.Backend != "b" {
		t.Errorf("race winner = %q, want b (the only verifier-passing backend — verifier judges, not the selector)", out.Backend)
	}

	// The race_outcome events name the two DISTINCT backends — the closed ledger loop.
	raced := map[string]bool{}
	var sawEscalate bool
	for _, e := range readEvents(t, logPath) {
		switch e.Kind {
		case "race_escalate":
			sawEscalate = true
		case "race_outcome":
			raced[e.Backend] = true
		}
	}
	if !sawEscalate {
		t.Error("expected a race_escalate event")
	}
	if !raced["a"] || !raced["b"] {
		t.Errorf("race_outcome backends = %v, want both distinct a and b", raced)
	}
}

// toStrings coerces a decoded JSON array (any of []any) to a []string for asserting
// the logged candidate order.
func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, _ := e.(string)
		out[i] = s
	}
	return out
}

// DEFAULT-OFF BYTE-IDENTICAL: with Backends empty (and NewEnvFor nil) the multi
// path is inert — multiBackend() is false — so executeSingle takes the existing
// NewEnv+Router path and raceEscalate races N copies of the one backend, exactly as
// before. This complements the unchanged orchestrator_test.go suite. Here we also
// prove the existing RaceN single-backend escalation still fires.
func TestDefaultOffUsesSingleBackendRaceEscalation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	// A backend whose single run fails verification but whose race copies pass, so
	// RaceN escalation recovers — the EXISTING single-backend race path.
	var calls int
	newEnv := func(string) agent.Env {
		calls++
		// First call is the single attempt (fails); the race copies pass.
		passed := calls > 1
		return agent.Env{Backend: &fakeBackend{name: "solo"}, Verifier: &fakeVerifier{passed: passed}}
	}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Log:      lg,
		NewEnv:   newEnv,
		RaceN:    2,
		// Backends empty + NewEnvFor nil ⇒ multiBackend() == false (default-off).
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "off-1", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if !out.Verified || out.Backend != "solo" {
		t.Errorf("default-off race escalation: got %+v, want verified solo", out)
	}

	// The race_escalate event carries the legacy {"n": RaceN} shape (no "backends"),
	// and every race_outcome names the SAME single backend — byte-identical.
	for _, e := range readEvents(t, logPath) {
		if e.Kind == "race_escalate" {
			if _, hasBackends := e.Detail["backends"]; hasBackends {
				t.Error("default-off race_escalate must NOT carry a backends list (that is the multi path)")
			}
			if n, _ := e.Detail["n"].(float64); int(n) != 2 {
				t.Errorf("default-off race_escalate n = %v, want 2 (RaceN)", e.Detail["n"])
			}
		}
		if e.Kind == "backend_select" {
			t.Error("default-off must NOT emit backend_select (that is the multi path)")
		}
	}
}

// Sanity: the verifier seam stays backend-independent on the multi path — the Env's
// own Verifier is what judges, not anything the Selector touched.
var _ verify.Verifier = (*fakeVerifier)(nil)
