// hermetic_test.go is the CI-visible sibling of TestNativeLoopConverges: it proves
// the assembled native agent loop actually CONVERGES — model turn → tool call →
// edit → verifier → repeat until pass → done DECIDED BY THE VERIFIER (I2), not by
// the model's self-report — while running in plain `go test ./...` (and thus
// `make verify` / CI) with NO network, NO API key, and NO container.
//
// How it stays hermetic yet real:
//   - The MODEL is a scripted fake (fakeConvergeModel): step 1 emits an `edit`
//     tool_use that fixes the deliberately-broken fixture; step 2 emits `finish`.
//     No provider call, no key.
//   - The EDIT is the REAL host-side structured tool (tools.EditTool), confined to
//     the throwaway worktree — so the tool call genuinely rewrites a file on disk,
//     not a mock.
//   - The VERIFIER is real (goTestVerifier): it runs `go test ./...` in the fixture
//     via os/exec on the host. Its pass/fail is a true function of the file's
//     current contents — RED before the edit, GREEN after — so convergence is the
//     actual signal, and completion is gated on THIS, never on the model's claim.
//   - The SANDBOX is a no-op box that satisfies sandbox.Sandbox but is never
//     invoked (the scripted model only calls `edit` and `finish`, never `run`), so
//     no container is needed.
//
// `go` must be on PATH for the real verify — CI has it — but nothing here reaches
// the network. TestNativeLoopConverges (the heavy real-API opt-in) is left as-is.
package smoke

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/sandbox"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// --- fakes ------------------------------------------------------------------

// fakeConvergeModel is a scripted, network-free model.Provider: it replays a
// pre-baked slice of Responses (each carrying tool_use blocks), one per Complete
// call, then returns a bare end_turn. This is the same test-double pattern the
// backend's own native_test.go uses — reproduced here so the smoke package need
// not import test-only symbols from another package.
type fakeConvergeModel struct {
	responses []model.Response
	i         int
	calls     int
}

func (m *fakeConvergeModel) Model() string { return "fake" }
func (m *fakeConvergeModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	m.calls++
	if m.i >= len(m.responses) {
		return model.Response{StopReason: "end_turn"}, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// noopBox satisfies sandbox.Sandbox for the loop's always-on `run` tool. The
// scripted model never calls `run` (it edits then finishes), so Exec is never hit;
// it exists only so Native has a non-nil Box. A call here would be a test bug, so
// it records that it fired for the assertion below.
type noopBox struct{ execed []string }

func (b *noopBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.execed = append(b.execed, cmd)
	return sandbox.Result{}, nil
}
func (b *noopBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *noopBox) Workdir() string { return "/work" }

// goTestVerifier is a REAL verifier: it runs `go test ./...` in dir on the host
// and reports pass/fail from the exit code. It is the genuine convergence signal —
// RED while the fixture bug is present, GREEN once the edit fixes it — and it is
// what decides "done" in the loop (I2), never the model's self-claim. No network:
// go builds+tests a self-contained local module.
type goTestVerifier struct {
	dir    string
	checks int
}

func (v *goTestVerifier) Check(ctx context.Context) (verify.Report, error) {
	v.checks++
	cmd := exec.CommandContext(ctx, "go", "test", "./...")
	cmd.Dir = v.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			// A non-exit failure (go missing, ctx cancelled) is a real error, not a
			// test result — surface it so the loop's verify path returns it.
			return verify.Report{}, err
		}
	}
	return verify.Report{Passed: cmd.ProcessState.ExitCode() == 0, Output: string(out)}, nil
}

// --- helpers ----------------------------------------------------------------

// stageFixture copies the failing-go fixture into a throwaway dir the EditTool can
// rewrite and the verifier can `go test`. It returns the staged dir. Nothing here
// touches the network; the fixture is a self-contained module.
func stageFixture(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "work")
	src := filepath.Join(repoRoot(t), "test", "fixtures", "failing-go")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}
	return dst
}

// mathxContains reports whether the fixture's mathx.go currently contains needle —
// used to prove the file was (or was not) actually edited on disk.
func mathxContains(t *testing.T, dir, needle string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "mathx.go"))
	if err != nil {
		t.Fatalf("read mathx.go: %v", err)
	}
	return strings.Contains(string(b), needle)
}

// editUse builds an `edit` tool_use block. The edit tool's input carries an int-free
// object, so a small marshal here keeps the block construction one-liner.
func editUse(id, path, old, new string) model.Block {
	in, _ := json.Marshal(map[string]string{"path": path, "old": old, "new": new})
	return model.Block{Type: "tool_use", ID: id, Name: "edit", Input: in}
}

func finishUse(id, summary string) model.Block {
	in, _ := json.Marshal(map[string]string{"summary": summary})
	return model.Block{Type: "tool_use", ID: id, Name: "finish", Input: in}
}

func openEventLog(t *testing.T) (*eventlog.Log, func() string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log, func() string {
		b, _ := os.ReadFile(path)
		return string(b)
	}
}

// --- the convergence test (positive) ----------------------------------------

// TestNativeLoopConvergesHermetic drives the WHOLE native backend.CodingBackend
// surface with model-free, network-free, container-free fakes and asserts it
// converges: the scripted model fixes the fixture with a REAL edit, then finishes;
// the REAL verifier — RED before the edit, GREEN after — is what decides done (I2).
func TestNativeLoopConvergesHermetic(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; the real verifier cannot run") // never true in CI
	}
	dir := stageFixture(t)

	// Sanity: the verifier is genuinely RED before any edit — otherwise "converges"
	// would be vacuous (the fixture must actually be broken to start).
	pre := &goTestVerifier{dir: dir}
	if rep, err := pre.Check(context.Background()); err != nil || rep.Passed {
		t.Fatalf("fixture must start RED (broken); Check passed=%v err=%v", rep.Passed, err)
	}

	log, readLog := openEventLog(t)
	box := &noopBox{}
	model0 := &fakeConvergeModel{responses: []model.Response{
		// Step 1: fix the bug with the real host-side edit tool.
		{Content: []model.Block{editUse("u1", "mathx.go", "a - b // BUG: should be a + b", "a + b")}, StopReason: "tool_use"},
		// Step 2: declare done — the VERIFIER, not this claim, decides completion.
		{Content: []model.Block{finishUse("u2", "fixed Add to return a + b")}, StopReason: "tool_use"},
	}}
	ver := &goTestVerifier{dir: dir}

	// Drive it through the frozen backend.CodingBackend contract (Run(ctx, Task)).
	var be backend.CodingBackend = &backend.Native{
		Model:    model0,
		Box:      box,
		Verifier: ver,
		Log:      log,
		Tools:    tools.NewRegistry(tools.EditTool{}), // the REAL, worktree-confined edit
		MaxSteps: 6,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := be.Run(ctx, backend.Task{ID: "hermetic", Goal: "Fix Add so go test passes.", Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1) The loop CONVERGED: a verified/passed Result.
	if !res.SelfClaimed {
		t.Fatalf("loop did not converge: Result.SelfClaimed=false, summary=%q", res.Summary)
	}

	// 2) The fixture file was ACTUALLY edited on disk (the bug is gone, the fix is in).
	if mathxContains(t, dir, "a - b") {
		t.Error("the buggy line is still present — the edit did not reach disk")
	}
	if !mathxContains(t, dir, "return a + b") {
		t.Error("the fix (a + b) is not in mathx.go — the edit did not reach disk")
	}

	// 3) Completion was GATED ON THE VERIFIER (I2), not the model's self-report:
	//    the verifier was actually run (its Check fired), and a `verify` verdict was
	//    recorded in the append-only log. The model finishing does not, by itself,
	//    return SelfClaimed — only a PASSING verifier does (asserted negatively below).
	if ver.checks == 0 {
		t.Error("the verifier was never consulted — completion cannot have been gated on it")
	}
	if lg := readLog(); !strings.Contains(lg, `"verify"`) {
		t.Error("no verify verdict recorded in the event log (I2/I5) — completion was not verifier-gated")
	}

	// The real container path was never taken: the scripted model edited via the
	// structured tool and finished; it never emitted a `run` shell command.
	if len(box.execed) != 0 {
		t.Errorf("the sandbox shell was invoked (%v); this test must not need a container", box.execed)
	}
}

// --- the verifier-authority test (negative) ---------------------------------

// TestNativeLoopVerifierGatesCompletion is the I2 authority check: when the model
// finishes WITHOUT fixing the file, the verifier NEVER passes, so the loop must NOT
// report success — no matter how confidently the model self-claims done. This is the
// exact inverse of the positive case and proves the verifier, not the self-report,
// decides "done". Because completion never comes, the loop exhausts its (small)
// budget and returns an unverified Result — the file is left untouched.
func TestNativeLoopVerifierGatesCompletion(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; the real verifier cannot run") // never true in CI
	}
	dir := stageFixture(t)

	log, readLog := openEventLog(t)
	// The model finishes on EVERY turn but never edits the bug — the verifier stays
	// RED forever. MaxSteps is small so the run terminates quickly on budget.
	model0 := &fakeConvergeModel{responses: []model.Response{
		{Content: []model.Block{finishUse("u1", "I believe this is done")}, StopReason: "tool_use"},
		{Content: []model.Block{finishUse("u2", "still confident it's done")}, StopReason: "tool_use"},
		{Content: []model.Block{finishUse("u3", "definitely done now")}, StopReason: "tool_use"},
	}}
	ver := &goTestVerifier{dir: dir}
	n := &backend.Native{
		Model:    model0,
		Box:      &noopBox{},
		Verifier: ver,
		Log:      log,
		Tools:    tools.NewRegistry(tools.EditTool{}),
		MaxSteps: 3,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := n.Run(ctx, backend.Task{ID: "negative", Goal: "Do nothing useful.", Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The core I2 assertion: a run whose verifier NEVER passes must NOT report success,
	// even though the model self-claimed done on every turn.
	if res.SelfClaimed {
		t.Fatal("verifier never passed, yet the loop reported success — the self-claim was allowed to decide 'done' (I2 broken)")
	}

	// The verifier WAS consulted on the model's finish (and rejected it) — that is the
	// gate doing its job, not the loop skipping verify.
	if ver.checks == 0 {
		t.Error("the verifier was never consulted despite the model finishing")
	}
	if lg := readLog(); !strings.Contains(lg, `"verify"`) {
		t.Error("no verify verdict recorded (I2/I5)")
	}

	// The file is untouched: no edit was emitted, so the bug is still present. This
	// confirms the negative case fails for the RIGHT reason (nothing was fixed).
	if !mathxContains(t, dir, "a - b // BUG: should be a + b") {
		t.Error("the buggy fixture was modified in the negative case — the model was supposed to do nothing")
	}
}
