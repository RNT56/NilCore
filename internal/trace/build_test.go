package trace

import (
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/eventlog"
)

// writeLog writes a real, hash-chained event log to a temp file using the actual
// eventlog writer, so the chain verifies the same way production does. Each entry
// is (kind, detail); the task and backend default to "T" / "native" unless the
// detail carries a "_task" / "_backend" override (stripped before write). It
// returns the log path.
func writeLog(t *testing.T, entries []eventlog.Event) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, e := range entries {
		if e.Task == "" {
			e.Task = "T"
		}
		log.Append(e)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("freshly-written log does not verify: %v", err)
	}
	return path
}

// realisticRun is the canonical run the spec describes:
// task_start -> model_call -> tool_exec -> verify{false} -> advisor ->
// model_call -> tool_exec -> verify{true} -> gate{approved} -> integrate.
func realisticRun() []eventlog.Event {
	return []eventlog.Event{
		{Task: "T", Kind: "task_start", Detail: map[string]any{"goal": "make the widget green", "base_repo": "/repo"}},
		{Task: "T", Backend: "native", Kind: "model_call", Detail: map[string]any{"step": 0, "stop": "tool_use", "out_tokens": 42}},
		{Task: "T", Backend: "native", Kind: "tool_exec", Detail: map[string]any{"tool": "edit"}},
		{Task: "T", Backend: "native", Kind: "verify", Detail: map[string]any{"passed": false}},
		{Task: "T", Backend: "native", Kind: "advisor_consult", Detail: map[string]any{"calls": 1}},
		{Task: "T", Backend: "native", Kind: "model_call", Detail: map[string]any{"step": 1, "stop": "tool_use", "out_tokens": 17}},
		{Task: "T", Backend: "native", Kind: "tool_exec", Detail: map[string]any{"tool": "edit"}},
		{Task: "T", Backend: "native", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "T", Kind: "gate", Detail: map[string]any{"action": "merge task/T", "class": "irreversible", "allowed": true}},
		{Task: "T", Kind: "integration_merge", Detail: map[string]any{"branch": "task/T", "pre_sha": "aaa", "sha": "bbb"}},
	}
}

func TestBuild_CausalTreeGroupsModelThenTools(t *testing.T) {
	path := writeLog(t, realisticRun())
	tr, err := Build(path, "T")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The two model_call nodes each absorb their following tool_exec as a child,
	// so the top-level steps are: task_start, model(+tool), verify, advisor,
	// model(+tool), verify, gate, integrate = 8 steps.
	if got := len(tr.Steps); got != 8 {
		t.Fatalf("top-level steps = %d, want 8: %+v", got, kinds(tr.Steps))
	}

	// Find the first model turn and assert it grouped exactly one tool child.
	var firstModel *Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == "model_call" {
			firstModel = &tr.Steps[i]
			break
		}
	}
	if firstModel == nil {
		t.Fatal("no model_call step found")
	}
	if len(firstModel.Children) != 1 {
		t.Fatalf("first model turn children = %d, want 1", len(firstModel.Children))
	}
	if firstModel.Children[0].Kind != "tool_exec" {
		t.Fatalf("model child kind = %q, want tool_exec", firstModel.Children[0].Kind)
	}
	if firstModel.Children[0].Title != "ran tool: edit" {
		t.Fatalf("tool title = %q, want %q", firstModel.Children[0].Title, "ran tool: edit")
	}
}

func TestBuild_VerifyFailToAdvisorTransitionWhy(t *testing.T) {
	path := writeLog(t, realisticRun())
	tr, err := Build(path, "T")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The advisor node, sitting right after a red verify, must carry a recovery Why.
	var advisor *Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == "advisor_consult" {
			advisor = &tr.Steps[i]
		}
	}
	if advisor == nil {
		t.Fatal("no advisor step found")
	}
	if advisor.Why == "" {
		t.Fatal("advisor step has no Why (expected a verify-fail recovery link)")
	}
	if !contains(advisor.Why, "recover") || !contains(advisor.Why, "failed verify") {
		t.Fatalf("advisor Why = %q, want a recovery-after-failed-verify annotation", advisor.Why)
	}

	// The second model turn (after the advisor) should explain it is re-planning
	// with the advisor's guidance.
	models := stepsOfKind(tr.Steps, "model_call")
	if len(models) != 2 {
		t.Fatalf("model turns = %d, want 2", len(models))
	}
	if !contains(models[1].Why, "advisor") {
		t.Fatalf("second model Why = %q, want it to name the advisor guidance", models[1].Why)
	}
}

func TestBuild_VerifyPassWhyCountsPriorFailures(t *testing.T) {
	path := writeLog(t, realisticRun())
	tr, _ := Build(path, "T")
	verifies := stepsOfKind(tr.Steps, "verify")
	if len(verifies) != 2 {
		t.Fatalf("verify steps = %d, want 2", len(verifies))
	}
	if verifies[0].Title != "verify FAILED" {
		t.Fatalf("first verify title = %q, want %q", verifies[0].Title, "verify FAILED")
	}
	if verifies[1].Title != "verify PASSED" {
		t.Fatalf("second verify title = %q, want %q", verifies[1].Title, "verify PASSED")
	}
	if !contains(verifies[1].Why, "after 1 failed attempt") {
		t.Fatalf("passing verify Why = %q, want it to count the 1 prior failure", verifies[1].Why)
	}
}

func TestBuild_GateWhyExplainsIrreversibility(t *testing.T) {
	path := writeLog(t, realisticRun())
	tr, _ := Build(path, "T")
	var gate *Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == "gate" {
			gate = &tr.Steps[i]
		}
	}
	if gate == nil {
		t.Fatal("no gate step")
	}
	if gate.Title != "human gate: approved" {
		t.Fatalf("gate title = %q", gate.Title)
	}
	if !contains(gate.Why, "irreversible") || !contains(gate.Why, "human sign-off") {
		t.Fatalf("gate Why = %q, want an irreversibility + sign-off explanation", gate.Why)
	}
}

func TestBuild_CleanChainVerdict(t *testing.T) {
	path := writeLog(t, realisticRun())
	tr, _ := Build(path, "T")
	if !tr.ChainVerified {
		t.Fatal("ChainVerified = false on a clean log")
	}
	if tr.Verdict != "integrated — the verified branch was merged" {
		t.Fatalf("verdict = %q", tr.Verdict)
	}
	if tr.Goal != "make the widget green" {
		t.Fatalf("goal = %q", tr.Goal)
	}
	for _, s := range tr.Steps {
		if s.Untrusted {
			t.Fatalf("step %d untrusted on a clean chain", s.Seq)
		}
	}
}

func TestBuild_CorruptedChainFailsClosed(t *testing.T) {
	path := writeLog(t, realisticRun())

	// Tamper: flip a byte inside the first event's Detail so its hash no longer
	// matches, breaking the chain — exactly how a real tamper surfaces.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the goal text in-place (same length) so JSON stays valid but the
	// hash diverges.
	corrupt := replaceFirst(string(data), "make the widget green", "make the widget GREEN")
	if corrupt == string(data) {
		t.Fatal("test setup: tamper string not found")
	}
	if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	if eventlog.Verify(path) == nil {
		t.Fatal("test setup: tampered log still verifies")
	}

	tr, err := Build(path, "T")
	if err != nil {
		t.Fatalf("Build over a broken chain should still return a structural trace, got err: %v", err)
	}
	if tr.ChainVerified {
		t.Fatal("ChainVerified = true over a tampered log")
	}
	if tr.Verdict != brokenChainVerdict {
		t.Fatalf("verdict = %q, want the CHAIN BROKEN verdict", tr.Verdict)
	}
	// Structure is still present (fail closed on trust, not on visibility)...
	if len(tr.Steps) == 0 {
		t.Fatal("no structural steps over a broken chain — must still show structure for debugging")
	}
	// ...but every node, at every depth, is marked untrusted.
	assertAllUntrusted(t, tr.Steps)
}

func TestBuildAll_SplitsTasks(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "A", Kind: "task_start", Detail: map[string]any{"goal": "alpha"}},
		{Task: "A", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "B", Kind: "task_start", Detail: map[string]any{"goal": "beta"}},
		{Task: "B", Kind: "verify", Detail: map[string]any{"passed": false}},
		{Task: "A", Kind: "final_verify", Detail: map[string]any{"passed": true}},
	})
	traces, err := BuildAll(path)
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if len(traces) != 2 {
		t.Fatalf("traces = %d, want 2", len(traces))
	}
	// First-seen order: A then B.
	if traces[0].Task != "A" || traces[1].Task != "B" {
		t.Fatalf("task order = %q,%q, want A,B", traces[0].Task, traces[1].Task)
	}
	if traces[0].Goal != "alpha" || traces[1].Goal != "beta" {
		t.Fatalf("goals = %q,%q", traces[0].Goal, traces[1].Goal)
	}
	// A's events must not bleed into B's trace.
	for _, s := range traces[1].Steps {
		if s.Kind == "final_verify" {
			t.Fatal("task A's final_verify leaked into task B's trace")
		}
	}
	if !traces[0].ChainVerified || !traces[1].ChainVerified {
		t.Fatal("clean log should leave every split trace verified")
	}
}

func TestBuild_AllTasksMerged(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "A", Kind: "task_start", Detail: map[string]any{"goal": "alpha"}},
		{Task: "B", Kind: "task_start", Detail: map[string]any{"goal": "beta"}},
	})
	tr, err := Build(path, "*")
	if err != nil {
		t.Fatalf("Build *: %v", err)
	}
	if len(tr.Steps) != 2 {
		t.Fatalf("merged steps = %d, want 2", len(tr.Steps))
	}
	if tr.Task != "(all tasks)" {
		t.Fatalf("merged task label = %q", tr.Task)
	}
}

func TestBuild_RaceClusterCollapses(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "T", Kind: "task_start", Detail: map[string]any{"goal": "race"}},
		{Task: "T", Backend: "native", Kind: "race_outcome", Detail: map[string]any{"passed": false}},
		{Task: "T", Backend: "codex", Kind: "race_outcome", Detail: map[string]any{"passed": true}},
		{Task: "T", Backend: "claude", Kind: "race_outcome", Detail: map[string]any{"passed": false}},
	})
	tr, _ := Build(path, "T")
	var cluster *Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == "race_cluster" {
			cluster = &tr.Steps[i]
		}
	}
	if cluster == nil {
		t.Fatalf("no race_cluster node; steps: %v", kinds(tr.Steps))
	}
	if len(cluster.Children) != 3 {
		t.Fatalf("race cluster children = %d, want 3", len(cluster.Children))
	}
	if !contains(cluster.Title, "raced 3 backends") {
		t.Fatalf("cluster title = %q", cluster.Title)
	}
	if !contains(cluster.Why, "verifier picked") {
		t.Fatalf("cluster Why = %q, want it to credit the verifier's pick", cluster.Why)
	}
}

// --- test helpers -----------------------------------------------------------

func kinds(steps []Step) []string {
	var ks []string
	for _, s := range steps {
		ks = append(ks, s.Kind)
	}
	return ks
}

func stepsOfKind(steps []Step, kind string) []Step {
	var out []Step
	for _, s := range steps {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

func assertAllUntrusted(t *testing.T, steps []Step) {
	t.Helper()
	for _, s := range steps {
		if !s.Untrusted {
			t.Fatalf("step #%d (%s) not marked untrusted over a broken chain", s.Seq, s.Kind)
		}
		assertAllUntrusted(t, s.Children)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func replaceFirst(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
