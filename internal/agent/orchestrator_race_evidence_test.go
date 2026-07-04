package agent_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/verify"
)

// captureBackend records the exact Task it was handed, so a test can assert on
// the Constraints the race escalation threads into each candidate. Each racer
// gets its OWN instance (NewEnv/NewEnvFor build one per worktree), so recording
// needs no lock even though route.Race runs candidates concurrently.
type captureBackend struct {
	name string
	got  backend.Task
	ran  bool
}

func (c *captureBackend) Name() string { return c.name }
func (c *captureBackend) Run(_ context.Context, t backend.Task) (backend.Result, error) {
	c.ran = true
	c.got = t
	return backend.Result{Backend: c.name, Summary: "did work", SelfClaimed: true}, nil
}

// outputVerifier is a fakeVerifier with a configurable Output, so the test
// controls the verifier evidence (fail-class shape + tail) the escalation must
// thread to the racers.
type outputVerifier struct {
	passed bool
	output string
}

func (v *outputVerifier) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: v.passed, Output: v.output}, nil
}

// A failing first attempt that escalates to the single-path RaceN race must hand
// every racer the failed attempt's verifier evidence as ONE extra Constraints
// line: the structural fail-class (harness vocabulary) framing a guard-fenced,
// bounded tail of the verifier output. Without it the racers re-run the
// IDENTICAL blind Task and predictably repeat the first attempt's mistake.
func TestRaceCandidatesCarryFailureEvidence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)

	// The failing verifier's output: a "go test" first line (⇒ fail-class "test"),
	// >1500 bytes of padding (forces the tail bound to truncate the head away), and
	// a unique end marker that must survive inside the fence.
	failOutput := "go test ./...\n" + strings.Repeat("x", 2000) + "\nUNIQUE-TAIL-9137"

	// First NewEnv call is the single attempt (verify FAILS with the output above);
	// the race copies pass, so the race recovers and Execute returns verified.
	var backends []*captureBackend
	calls := 0
	newEnv := func(string) agent.Env {
		calls++
		cb := &captureBackend{name: "solo"}
		backends = append(backends, cb)
		if calls == 1 {
			return agent.Env{Backend: cb, Verifier: &outputVerifier{passed: false, output: failOutput}}
		}
		return agent.Env{Backend: cb, Verifier: &outputVerifier{passed: true}}
	}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv:   newEnv,
		RaceN:    2,
	}

	// Extra capacity on the caller's Constraints: if the escalation appended IN
	// PLACE instead of copying, the write would land in this backing array — the
	// re-slice check below would see it.
	orig := make([]string, 1, 8)
	orig[0] = "keep the diff minimal"

	out, err := orch.Execute(context.Background(),
		backend.Task{ID: "ev-1", Goal: "x", Constraints: orig})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Fatal("the race copies pass; expected a verified outcome")
	}
	if len(backends) != 3 {
		t.Fatalf("built %d backends, want 3 (1 single attempt + RaceN=2 racers)", len(backends))
	}

	// The FIRST attempt is blind — it runs before any failure exists.
	if got := backends[0].got.Constraints; len(got) != 1 || got[0] != orig[0] {
		t.Errorf("first attempt Constraints = %q, want only the original", got)
	}

	// Every RACER carries the original constraint plus exactly one evidence line.
	for i, cb := range backends[1:] {
		got := cb.got.Constraints
		if len(got) != 2 {
			t.Fatalf("racer %d Constraints = %q, want original + 1 evidence line", i, got)
		}
		if got[0] != orig[0] {
			t.Errorf("racer %d lost the original constraint: %q", i, got[0])
		}
		ev := got[1]
		// Harness-authored framing with the STRUCTURAL fail-class ("go test" ⇒ test).
		if !strings.Contains(ev, "a previous attempt failed verification (class=test)") {
			t.Errorf("racer %d evidence lacks the fail-class framing: %q", i, ev)
		}
		// The verifier output is untrusted (I7) and must sit INSIDE a guard fence.
		fenced := strings.Contains(ev, "<<<BEGIN UNTRUSTED DATA>>>") &&
			strings.Contains(ev, "<<<END UNTRUSTED DATA>>>")
		if !fenced {
			t.Errorf("racer %d evidence is not guard-fenced: %q", i, ev)
		}
		// Bounded tail: the END of the output survives, the head is truncated away.
		if !strings.Contains(ev, "UNIQUE-TAIL-9137") {
			t.Errorf("racer %d evidence lost the output tail: %q", i, ev)
		}
		if !strings.Contains(ev, "...(truncated)...") {
			t.Errorf("racer %d evidence >1500B output was not marked truncated: %q", i, ev)
		}
		if strings.Contains(ev, "go test ./...") {
			t.Errorf("racer %d evidence kept the output HEAD past the tail bound: %q", i, ev)
		}
	}

	// The caller's slice — including the spare capacity of its backing array —
	// was never mutated (withFailureEvidence copies, never appends in place).
	if len(orig) != 1 || orig[0] != "keep the diff minimal" {
		t.Errorf("caller Constraints mutated: %q", orig)
	}
	if spare := orig[:cap(orig)][1]; spare != "" {
		t.Errorf("escalation appended into the caller's backing array: %q", spare)
	}
}

// The multi-backend race gets the same evidence threading: after backend a's
// first attempt fails verification, BOTH distinct racers (a and b) receive the
// fenced evidence line, and the verifier still picks the winner (I2).
func TestMultiBackendRaceCandidatesCarryFailureEvidence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)

	// Per-name build history: a is built twice (first attempt + racer), b once.
	built := map[string][]*captureBackend{}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		Backends: []string{"a", "b"},
		NewEnvFor: func(_, name string) agent.Env {
			cb := &captureBackend{name: name}
			built[name] = append(built[name], cb)
			// a always fails verification (forces escalation; loses the race);
			// b passes — the verifier, not the ordering, picks it.
			return agent.Env{Backend: cb, Verifier: &outputVerifier{passed: name == "b", output: "checked"}}
		},
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "ev-2", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified || out.Backend != "b" {
		t.Fatalf("outcome = %+v, want verified winner b", out)
	}
	if len(built["a"]) != 2 || len(built["b"]) != 1 {
		t.Fatalf("built a=%d b=%d backends, want a=2 (attempt+racer) b=1 (racer)", len(built["a"]), len(built["b"]))
	}

	// First attempt (a) is blind; both racers carry the evidence line with the
	// fenced verifier output ("checked" ⇒ structurally unclassifiable ⇒ "other").
	if got := built["a"][0].got.Constraints; len(got) != 0 {
		t.Errorf("first attempt Constraints = %q, want none", got)
	}
	for _, cb := range []*captureBackend{built["a"][1], built["b"][0]} {
		got := cb.got.Constraints
		if len(got) != 1 {
			t.Fatalf("racer %s Constraints = %q, want exactly the evidence line", cb.name, got)
		}
		ev := got[0]
		if !strings.Contains(ev, "a previous attempt failed verification (class=other)") {
			t.Errorf("racer %s evidence lacks the fail-class framing: %q", cb.name, ev)
		}
		if !strings.Contains(ev, "<<<BEGIN UNTRUSTED DATA>>>") || !strings.Contains(ev, "checked") {
			t.Errorf("racer %s evidence lacks the fenced verifier output: %q", cb.name, ev)
		}
	}
}
