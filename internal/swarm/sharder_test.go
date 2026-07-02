package swarm

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/model"
	"nilcore/internal/planner"
	"nilcore/internal/sandbox"
)

// fakeModel returns a canned text completion — the planner's only input — so the
// PlanSharder tests are hermetic (no network).
type fakeModel struct{ text string }

func (fakeModel) Model() string { return "fake" }
func (f fakeModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: f.text}}}, nil
}

// fakeBox is a sandbox.Sandbox that returns a scripted Exec result, so FailureSharder
// is tested over a controlled verify output with no real container.
type fakeBox struct {
	workdir string
	out     string
	exit    int
}

func (b fakeBox) Workdir() string { return b.workdir }
func (b fakeBox) Exec(context.Context, string) (sandbox.Result, error) {
	return sandbox.Result{Stdout: b.out, ExitCode: b.exit}, nil
}
func (b fakeBox) ExecWithEnv(context.Context, string, map[string]string) (sandbox.Result, error) {
	return sandbox.Result{Stdout: b.out, ExitCode: b.exit}, nil
}

// TestListSharderNamespacedNoModel asserts ListSharder fans N lines into N namespaced
// shards in order, carrying caller-supplied routing, with no model call.
func TestListSharderNamespacedNoModel(t *testing.T) {
	s := ListSharder{
		Lines: []string{"company A", "", "# a comment", "company B", "company C"},
		Kind:  artifact.KindReport,
		Pack:  "finance",
		Role:  "researcher",
		Tier:  "strong",
	}
	shards, err := s.Shards(context.Background(), "audit revenue", "run1")
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("got %d shards, want 3 (blanks/comments dropped)", len(shards))
	}
	wantIDs := []string{"swarm-run1-0", "swarm-run1-1", "swarm-run1-2"}
	for i, sh := range shards {
		if sh.ID != wantIDs[i] {
			t.Errorf("shard[%d].ID = %q, want %q", i, sh.ID, wantIDs[i])
		}
		// REGRESSION (the false-convergence blocker): a shard owns ONE artifact at
		// .nilcore/artifacts/<shard id>.json and the convergence model keys artifact id ==
		// shard id, but the artifact store rejects a '/'-containing id. If the shard id were
		// not a valid single-component artifact id, the per-shard artifact read/write/verify
		// would silently fail and a failed run would falsely converge green. Assert it has no
		// path separator and no leading dot.
		if strings.ContainsRune(sh.ID, '/') || strings.HasPrefix(sh.ID, ".") {
			t.Errorf("shard[%d].ID %q must be a valid single-component artifact id", i, sh.ID)
		}
		if sh.Kind != artifact.KindReport || sh.Pack != "finance" || sh.Role != "researcher" || sh.Tier != "strong" {
			t.Errorf("shard[%d] routing not carried: %+v", i, sh)
		}
		if sh.State != ShardQueued {
			t.Errorf("shard[%d].State = %q, want queued", i, sh.State)
		}
	}
	// The unit line is woven into the per-shard goal, and the run goal frames it.
	if !strings.Contains(shards[0].Goal, "company A") || !strings.Contains(shards[0].Goal, "audit revenue") {
		t.Errorf("shard goal missing line or run goal: %q", shards[0].Goal)
	}
}

// TestListSharderEmptyIsNoError asserts an all-blank list yields zero shards without an
// error (an empty list is a legitimate, if useless, operator input — distinct from a
// parse failure).
func TestListSharderEmptyIsNoError(t *testing.T) {
	s := ListSharder{Lines: []string{"", "  ", "# only comments"}}
	shards, err := s.Shards(context.Background(), "g", "run1")
	if err != nil {
		t.Fatalf("empty list should not error: %v", err)
	}
	if len(shards) != 0 {
		t.Errorf("got %d shards, want 0", len(shards))
	}
}

// TestPlanSharderCarriesDeps asserts a valid plan maps DependsOn onto Shard.Deps,
// re-namespaced to the run's shard ids.
func TestPlanSharderCarriesDeps(t *testing.T) {
	m := fakeModel{text: `{"goal":"ship feature","tasks":[
		{"id":"t1","goal":"write failing test","depends_on":[],"acceptance":"test fails"},
		{"id":"t2","goal":"implement","depends_on":["t1"],"acceptance":"test passes"}]}`}
	s := PlanSharder{Model: m, Kind: artifact.KindSpec, Pack: "code", Role: "implementer"}
	shards, err := s.Shards(context.Background(), "ship feature", "run9")
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2", len(shards))
	}
	if shards[0].ID != "swarm-run9-0" || shards[1].ID != "swarm-run9-1" {
		t.Errorf("ids = %q,%q", shards[0].ID, shards[1].ID)
	}
	if len(shards[1].Deps) != 1 || shards[1].Deps[0] != "swarm-run9-0" {
		t.Errorf("shard[1].Deps = %v, want [swarm-run9-0]", shards[1].Deps)
	}
	if shards[0].Pack != "code" || shards[0].Kind != artifact.KindSpec {
		t.Errorf("routing not applied: %+v", shards[0])
	}
}

// TestTreeSharderReNamespacesDeps asserts a PRE-BUILT tree (no model call — the flows
// path) maps each task to a run-namespaced shard and rewrites DependsOn (plan-task ids)
// onto Shard.Deps (shard ids), including a task that depends on TWO others. This is the
// structure-preservation guarantee `nilcore flows run` relies on: the flow's DAG becomes
// real Shard.Deps the runner honors, not a flattened goal list.
func TestTreeSharderReNamespacesDeps(t *testing.T) {
	tree := planner.Tree{
		Goal: "ship it — agentic-flows source: demo@1.0.0",
		Tasks: []planner.PlanTask{
			{ID: "scaffold", Goal: "scaffold the module", DependsOn: nil},
			{ID: "impl", Goal: "implement on the scaffold", DependsOn: []string{"scaffold"}},
			{ID: "wire", Goal: "wire impl into the scaffold", DependsOn: []string{"scaffold", "impl"}},
		},
	}
	s := TreeSharder{Tree: tree, Kind: artifact.KindSpec, Pack: "code", Role: "implementer", Tier: "strong"}
	shards, err := s.Shards(context.Background(), "ignored", "runF")
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("got %d shards, want 3", len(shards))
	}
	wantIDs := []string{"swarm-runF-0", "swarm-runF-1", "swarm-runF-2"}
	for i, sh := range shards {
		if sh.ID != wantIDs[i] {
			t.Errorf("shard[%d].ID = %q, want %q", i, sh.ID, wantIDs[i])
		}
		if sh.Goal != tree.Tasks[i].Goal || sh.Input != tree.Tasks[i].Goal {
			t.Errorf("shard[%d] goal/input = %q/%q, want %q", i, sh.Goal, sh.Input, tree.Tasks[i].Goal)
		}
		if sh.Kind != artifact.KindSpec || sh.Pack != "code" || sh.Role != "implementer" || sh.Tier != "strong" {
			t.Errorf("shard[%d] routing not carried: %+v", i, sh)
		}
		if sh.State != ShardQueued {
			t.Errorf("shard[%d].State = %q, want queued", i, sh.State)
		}
	}
	if len(shards[0].Deps) != 0 {
		t.Errorf("root shard Deps = %v, want none", shards[0].Deps)
	}
	if len(shards[1].Deps) != 1 || shards[1].Deps[0] != "swarm-runF-0" {
		t.Errorf("shard[1].Deps = %v, want [swarm-runF-0]", shards[1].Deps)
	}
	if len(shards[2].Deps) != 2 || shards[2].Deps[0] != "swarm-runF-0" || shards[2].Deps[1] != "swarm-runF-1" {
		t.Errorf("shard[2].Deps = %v, want [swarm-runF-0 swarm-runF-1]", shards[2].Deps)
	}
}

// TestPlanSharderInvalidPlanErrors asserts an unparseable/invalid plan is an ERROR, not
// a silent empty set — exercising the planner.Validate reuse (fail-closed).
func TestPlanSharderInvalidPlanErrors(t *testing.T) {
	cases := map[string]string{
		"non-JSON":           "I cannot do that",
		"missing acceptance": `{"goal":"x","tasks":[{"id":"t1","goal":"do","depends_on":[],"acceptance":""}]}`,
		"no tasks":           `{"goal":"x","tasks":[]}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			s := PlanSharder{Model: fakeModel{text: text}}
			shards, err := s.Shards(context.Background(), "x", "run1")
			if err == nil {
				t.Errorf("invalid plan must error, got %d shards", len(shards))
			}
			if shards != nil {
				t.Errorf("invalid plan must yield no shards, got %+v", shards)
			}
		})
	}
}

// TestPlanSharderNilModelErrors asserts a missing model is a setup error, never a
// silent empty plan.
func TestPlanSharderNilModelErrors(t *testing.T) {
	s := PlanSharder{}
	if _, err := s.Shards(context.Background(), "x", "run1"); err == nil {
		t.Error("nil model must error")
	}
}

// TestFailureSharderOneShardPerRedTest asserts FailureSharder parses failing test names
// from the box output and emits one shard per failure in order, de-duplicated.
func TestFailureSharderOneShardPerRedTest(t *testing.T) {
	out := strings.Join([]string{
		"=== RUN   TestAlpha",
		"--- FAIL: TestAlpha (0.00s)",
		"=== RUN   TestBeta",
		"--- FAIL: TestBeta (0.01s)",
		"--- FAIL: TestAlpha (0.00s)", // duplicate — must not double-count
		"FAIL",
	}, "\n")
	s := FailureSharder{Box: fakeBox{workdir: t.TempDir(), out: out, exit: 1}, Pack: "code", Role: "fixer"}
	shards, err := s.Shards(context.Background(), "fix the suite", "run1")
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2 (one per distinct red test)", len(shards))
	}
	if !strings.Contains(shards[0].Goal, "TestAlpha") || !strings.Contains(shards[1].Goal, "TestBeta") {
		t.Errorf("shard goals = %q, %q", shards[0].Goal, shards[1].Goal)
	}
	if shards[0].Pack != "code" {
		t.Errorf("routing not carried: %+v", shards[0])
	}
}

// TestFailureSharderGreenSuiteNoShards asserts a passing suite (exit 0) yields zero
// shards — there is nothing to fix.
func TestFailureSharderGreenSuiteNoShards(t *testing.T) {
	s := FailureSharder{Box: fakeBox{workdir: t.TempDir(), out: "ok", exit: 0}}
	shards, err := s.Shards(context.Background(), "g", "run1")
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(shards) != 0 {
		t.Errorf("green suite must yield 0 shards, got %d", len(shards))
	}
}

// TestFailureSharderUnparseableRedFallsBack asserts a red suite whose output has no
// recognizable per-test lines falls back to a SINGLE whole-suite shard (never a silent
// empty set that would read as "no failures").
func TestFailureSharderUnparseableRedFallsBack(t *testing.T) {
	s := FailureSharder{Box: fakeBox{workdir: t.TempDir(), out: "build failed: undefined symbol", exit: 2}}
	shards, err := s.Shards(context.Background(), "g", "run1")
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(shards) != 1 {
		t.Fatalf("unparseable red must fall back to 1 shard, got %d", len(shards))
	}
	if !strings.Contains(shards[0].Goal, "verify command") {
		t.Errorf("fallback shard goal = %q", shards[0].Goal)
	}
}

// TestParseFailuresPatterns covers the per-runner failure-line shapes parseFailures
// recognizes (Go, pytest, generic FAIL:, TAP).
func TestParseFailuresPatterns(t *testing.T) {
	in := strings.Join([]string{
		"--- FAIL: TestGo (0.00s)",
		"FAILED tests/test_x.py::test_foo",
		"FAIL: SomeJavaTest",
		"not ok 1 - tap style check",
		"ok 2 - passing tap line",
		"random noise that matches nothing",
	}, "\n")
	got := parseFailures(in)
	want := []string{"TestGo", "tests/test_x.py::test_foo", "SomeJavaTest", "tap style check"}
	if len(got) != len(want) {
		t.Fatalf("parseFailures = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseFailures[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
