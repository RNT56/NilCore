package integrate

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// fileVerifier is a faithful stand-in for the project verifier: it runs over the
// actual merged worktree directory, so the test exercises the real git merge /
// commit / reset machinery rather than mocking it out. It passes only when the
// combined contents of `marker` satisfy `ok` — letting a test model "green alone
// but red combined" purely through file state, exactly as a real verifier would
// see it after a merge.
type fileVerifier struct {
	dir    string
	marker string
	ok     func(content string) bool
}

func (v *fileVerifier) Check(context.Context) (verify.Report, error) {
	b, err := os.ReadFile(filepath.Join(v.dir, v.marker))
	if err != nil {
		// A missing marker file is a red tree, not a verifier fault.
		return verify.Report{Passed: false, Output: err.Error()}, nil
	}
	return verify.Report{Passed: v.ok(string(b)), Output: string(b)}, nil
}

// sumVerifier passes only while the integers in all count_* files in the merged
// tree sum to <= max. It models a constraint that two branches each satisfy alone
// but violate together — the green-alone-red-combined case — through file state.
type sumVerifier struct {
	dir string
	max int
}

func (v *sumVerifier) Check(context.Context) (verify.Report, error) {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return verify.Report{}, err
	}
	total := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "count_") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(v.dir, e.Name()))
		if rerr != nil {
			return verify.Report{}, rerr
		}
		n, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr != nil {
			return verify.Report{Passed: false, Output: perr.Error()}, nil
		}
		total += n
	}
	return verify.Report{Passed: total <= v.max, Output: strconv.Itoa(total)}, nil
}

// hgit runs a hardened git command in dir for test setup, failing the test on
// error. Using the same clamp as the package keeps setup hermetic (no host git
// config / hooks leak in).
func hgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append(tools.HardenArgs(),
		append([]string{"-c", "user.email=test@nilcore.local", "-c", "user.name=test"}, args...)...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = tools.HardenedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// mergeHeadExists reports whether the worktree at dir is mid-merge (MERGE_HEAD
// present). It tolerates the non-zero exit git returns when the ref is absent,
// so it is the right check for "the abort cleared the merge state".
func mergeHeadExists(t *testing.T, dir string) bool {
	t.Helper()
	cmd := exec.Command("git", append(tools.HardenArgs(), "rev-parse", "-q", "--verify", "MERGE_HEAD")...)
	cmd.Dir = dir
	cmd.Env = tools.HardenedEnv()
	out, err := cmd.CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// writeFile writes content to dir/name, creating parent dirs.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// baseRepo creates a git repo with an initial commit so worktrees have a HEAD.
func baseRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	hgit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "README", "base\n")
	hgit(t, dir, "add", "-A")
	hgit(t, dir, "commit", "-q", "-m", "base")
	return dir
}

// branchFrom creates branch off "main" with the given files committed, then
// returns to main. The files map is path→content.
func branchFrom(t *testing.T, repo, branch string, files map[string]string) {
	t.Helper()
	hgit(t, repo, "checkout", "-q", "-b", branch, "main")
	for name, content := range files {
		writeFile(t, repo, name, content)
	}
	hgit(t, repo, "add", "-A")
	hgit(t, repo, "commit", "-q", "-m", "branch "+branch)
	hgit(t, repo, "checkout", "-q", "main")
}

// baseHead returns the current sha of the repo's main branch.
func baseHead(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(hgit(t, repo, "rev-parse", "main"))
}

// testLog opens a fresh event log in a temp dir and returns it plus a reader of
// the recorded event kinds.
func testLog(t *testing.T) (*eventlog.Log, func() []eventlog.Event) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	read := func() []eventlog.Event {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		defer f.Close()
		var out []eventlog.Event
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e eventlog.Event
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("decode event %q: %v", sc.Text(), err)
			}
			out = append(out, e)
		}
		return out
	}
	return log, read
}

func kinds(events []eventlog.Event) []string {
	ks := make([]string, len(events))
	for i, e := range events {
		ks[i] = e.Kind
	}
	return ks
}

func hasKind(events []eventlog.Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// newEnvFor returns a NewEnv factory whose verifier passes when the named marker
// file's content satisfies ok.
func newEnvFor(marker string, ok func(string) bool) func(dir string) Env {
	return func(dir string) Env {
		return Env{Verifier: &fileVerifier{dir: dir, marker: marker, ok: ok}}
	}
}

// TestGreenAloneRedCombined is the headline acceptance: two branches each
// verifier-green on their own, but red when both are merged, must leave the
// second branch rolled back to the pre-merge SHA with Verified:false — the tip
// stays the verified first merge.
func TestGreenAloneRedCombined(t *testing.T) {
	repo := baseRepo(t)
	// Each branch adds one unit in its OWN file (so the merge is conflict-free).
	// The verifier sums both: a alone = 1 (green), b alone = 1 (green), but a+b
	// merged = 2 (red). The redness is semantic — invisible to git, caught only by
	// re-verify on the merged tree, which is exactly what the integrator guards.
	branchFrom(t, repo, "task/a", map[string]string{"count_a": "1\n"})
	branchFrom(t, repo, "task/b", map[string]string{"count_b": "1\n"})

	sumOK := func(dir string) Env {
		return Env{Verifier: &sumVerifier{dir: dir, max: 1}}
	}
	log, readEvents := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		NewEnv:   sumOK,
		Log:      log,
	}

	wt, results, err := it.Integrate(context.Background(), []MergeItem{
		{ID: "a", Branch: "task/a"},
		{ID: "b", Branch: "task/b"},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer wt.Cleanup()

	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	// First branch keeps the tip green.
	if !results[0].Merged || !results[0].Verified {
		t.Errorf("branch a: want merged+verified, got %+v", results[0])
	}
	// Second branch merges cleanly but turns the tree red → rolled back.
	if !results[1].Merged {
		t.Errorf("branch b: expected a clean merge, got %+v", results[1])
	}
	if results[1].Verified {
		t.Errorf("branch b: want Verified:false (red combined), got true")
	}
	if !results[1].Escalate {
		t.Errorf("branch b: want Escalate:true for a re-plan signal")
	}
	// Rollback target is exactly the tip after a (b.PreSHA), and the worktree HEAD
	// is restored to it — no unverified state on the tip (the convergence invariant).
	if results[1].SHA != results[1].PreSHA {
		t.Errorf("branch b: SHA should be the pre-merge tip after rollback: SHA=%s PreSHA=%s", results[1].SHA, results[1].PreSHA)
	}
	tip, herr := wt.Head(context.Background())
	if herr != nil {
		t.Fatal(herr)
	}
	if tip != results[1].PreSHA {
		t.Errorf("tip after rollback = %s, want pre-merge SHA %s", tip, results[1].PreSHA)
	}
	if tip != results[0].SHA {
		t.Errorf("tip should equal the kept first-merge SHA %s, got %s", results[0].SHA, tip)
	}

	events := readEvents()
	if !hasKind(events, "integration_start") {
		t.Errorf("missing integration_start; kinds=%v", kinds(events))
	}
	if !hasKind(events, "integration_verify") {
		t.Errorf("missing integration_verify; kinds=%v", kinds(events))
	}
	if !hasKind(events, "integration_rollback") {
		t.Errorf("missing integration_rollback; kinds=%v", kinds(events))
	}
}

// TestConflictAbortsCleanly checks that a true merge conflict is aborted (not
// committed), leaves the worktree clean and on the pre-merge tip, preserves the
// branch in the base repo for retry, and is reported as a conflict to escalate.
func TestConflictAbortsCleanly(t *testing.T) {
	repo := baseRepo(t)
	// Both branches edit the SAME file differently → a real merge conflict.
	branchFrom(t, repo, "task/a", map[string]string{"shared.txt": "from-a\n"})
	branchFrom(t, repo, "task/b", map[string]string{"shared.txt": "from-b\n"})

	log, readEvents := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		NewEnv:   newEnvFor("shared.txt", func(string) bool { return true }),
		Log:      log,
	}

	wt, results, err := it.Integrate(context.Background(), []MergeItem{
		{ID: "a", Branch: "task/a"},
		{ID: "b", Branch: "task/b"},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer wt.Cleanup()

	if !results[0].Verified {
		t.Fatalf("branch a should merge green first: %+v", results[0])
	}
	if !results[1].Conflict {
		t.Errorf("branch b: want Conflict:true, got %+v", results[1])
	}
	if results[1].Merged {
		t.Errorf("branch b: a conflict must not be recorded as merged")
	}
	if !results[1].Escalate {
		t.Errorf("branch b: a conflict must escalate")
	}
	// The tree is clean after the abort (a left-over MERGE_HEAD or staged conflict
	// would mean the abort did not restore the pre-merge state).
	status := strings.TrimSpace(hgit(t, wt.Path(), "status", "--porcelain"))
	if status != "" {
		t.Errorf("worktree not clean after merge --abort:\n%s", status)
	}
	// MERGE_HEAD lives in the worktree's gitdir; its absence proves the abort
	// finished and the tree is no longer mid-merge. rev-parse exits non-zero when
	// the ref is absent (the success case here), so this check must not be fatal.
	if mergeHeadExists(t, wt.Path()) {
		t.Errorf("MERGE_HEAD still present after abort")
	}
	tip, _ := wt.Head(context.Background())
	if tip != results[1].PreSHA {
		t.Errorf("tip after abort = %s, want pre-merge SHA %s", tip, results[1].PreSHA)
	}
	// The conflicting branch is preserved in the base repo for a re-plan/retry.
	if out := strings.TrimSpace(hgit(t, repo, "rev-parse", "-q", "--verify", "task/b")); out == "" {
		t.Errorf("branch task/b was not preserved after a conflict")
	}
	if !hasKind(readEvents(), "integration_conflict") {
		t.Errorf("missing integration_conflict event")
	}
}

// TestNeverLandsToBase asserts the integrator does not advance the base branch:
// after a fully-green integration, the base repo's main is exactly where it
// started — promotion is the project loop's gated step, never the integrator's.
func TestNeverLandsToBase(t *testing.T) {
	repo := baseRepo(t)
	branchFrom(t, repo, "task/a", map[string]string{"a.txt": "x\n"})
	branchFrom(t, repo, "task/b", map[string]string{"b.txt": "y\n"})
	startMain := baseHead(t, repo)

	log, _ := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		NewEnv:   newEnvFor("README", func(string) bool { return true }), // always green
		Log:      log,
	}

	wt, results, err := it.Integrate(context.Background(), []MergeItem{
		{ID: "a", Branch: "task/a"},
		{ID: "b", Branch: "task/b"},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer wt.Cleanup()

	for _, r := range results {
		if !r.Verified {
			t.Fatalf("expected all green, got %+v", r)
		}
	}
	// Base main must be untouched — the integrator never lands.
	if got := baseHead(t, repo); got != startMain {
		t.Errorf("base main moved: %s != %s (integrator must never land to base)", got, startMain)
	}
	// The integration worktree tip carries BOTH branches' files (the green result).
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(wt.Path(), f)); err != nil {
			t.Errorf("integration tip missing %s: %v", f, err)
		}
	}
	// No integration_landed event is ever emitted by the integrator.
	// (landing is the project loop's gated promote, not this package's concern)
}

// TestAllGreenSequential checks the happy path: independent green branches all
// merge and verify in order, each becoming the new tip.
func TestAllGreenSequential(t *testing.T) {
	repo := baseRepo(t)
	branchFrom(t, repo, "task/a", map[string]string{"a.txt": "1\n"})
	branchFrom(t, repo, "task/b", map[string]string{"b.txt": "2\n"})
	branchFrom(t, repo, "task/c", map[string]string{"c.txt": "3\n"})

	log, readEvents := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		NewEnv:   newEnvFor("README", func(string) bool { return true }),
		Log:      log,
	}
	wt, results, err := it.Integrate(context.Background(), []MergeItem{
		{ID: "a", Branch: "task/a"},
		{ID: "b", Branch: "task/b"},
		{ID: "c", Branch: "task/c"},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer wt.Cleanup()

	prev := ""
	for i, r := range results {
		if !r.Merged || !r.Verified || r.Escalate {
			t.Errorf("result %d not clean-green: %+v", i, r)
		}
		if r.SHA == r.PreSHA {
			t.Errorf("result %d: a kept merge must advance the tip", i)
		}
		if prev != "" && r.PreSHA != prev {
			t.Errorf("result %d: pre-sha %s should chain from previous tip %s", i, r.PreSHA, prev)
		}
		prev = r.SHA
	}
	verifies := 0
	for _, e := range readEvents() {
		if e.Kind == "integration_verify" {
			verifies++
		}
	}
	if verifies != 3 {
		t.Errorf("want 3 integration_verify events, got %d", verifies)
	}
}

// TestEmptyOrderReturnsBaseTip checks that integrating nothing yields a usable
// worktree sitting on the base tip (a verified state) and no per-item results.
func TestEmptyOrderReturnsBaseTip(t *testing.T) {
	repo := baseRepo(t)
	log, readEvents := testLog(t)
	it := &Integrator{
		BaseRepo: repo,
		NewEnv:   newEnvFor("README", func(string) bool { return true }),
		Log:      log,
	}
	wt, results, err := it.Integrate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	defer wt.Cleanup()
	if len(results) != 0 {
		t.Errorf("want 0 results for empty order, got %d", len(results))
	}
	tip, _ := wt.Head(context.Background())
	if tip != baseHead(t, repo) {
		t.Errorf("empty integration tip %s != base head %s", tip, baseHead(t, repo))
	}
	if !hasKind(readEvents(), "integration_start") {
		t.Errorf("integration_start should still be logged for an empty order")
	}
}

// TestConfig guards the setup-error contract: a missing NewEnv or BaseRepo is a
// program fault returned as an error (not a silent nil-worktree).
func TestConfig(t *testing.T) {
	tests := []struct {
		name string
		it   *Integrator
	}{
		{"no NewEnv", &Integrator{BaseRepo: "x"}},
		{"no BaseRepo", &Integrator{NewEnv: newEnvFor("README", func(string) bool { return true })}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wt, _, err := tc.it.Integrate(context.Background(), nil)
			if err == nil {
				t.Fatalf("want error for %s", tc.name)
			}
			if wt != nil {
				t.Errorf("want nil worktree on setup error")
			}
		})
	}
}
