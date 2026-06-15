package project

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/advisor"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// --- hermetic helpers --------------------------------------------------------

// hgit runs a hardened git command in dir for test setup/assertions, failing the
// test on error. It uses the same clamp as the package so setup is hermetic and
// never depends on host git config (matching the integrate/worktree tests).
func hgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append(tools.HardenArgs(), args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = tools.HardenedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// gitRepoWith makes a real git repo with one commit and the given files, so a test
// can drive NeedsBootstrap against a repo that already has a HEAD and a layout.
func gitRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	hgit(t, dir, "init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	hgit(t, dir, "add", "-A")
	hgit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "base")
	return dir
}

// hasHeadTest reports whether a repo has a resolvable HEAD (a committed state).
func hasHeadTest(t *testing.T, dir string) bool {
	t.Helper()
	full := append(tools.HardenArgs(), "rev-parse", "--verify", "--quiet", "HEAD")
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = tools.HardenedEnv()
	return cmd.Run() == nil
}

// --- NeedsBootstrap: the three greenfield triggers ---------------------------

func TestNeedsBootstrap_Triggers(t *testing.T) {
	t.Run("empty repo path triggers", func(t *testing.T) {
		if !NeedsBootstrap("") {
			t.Error("empty Repo must trigger bootstrap")
		}
	})

	t.Run("a non-git directory triggers", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644)
		if !NeedsBootstrap(dir) {
			t.Error("a non-git dir must trigger bootstrap")
		}
	})

	t.Run("a git repo whose only verifier is the vacuous true triggers", func(t *testing.T) {
		// A README-only git repo: it has a HEAD but verify.Detect returns "true"
		// (no recognizable layout) → the only check is a vacuous pass → bootstrap.
		dir := gitRepoWith(t, map[string]string{"README": "x"})
		if verify.Detect(dir) != "true" {
			t.Fatalf("precondition: Detect should be vacuous, got %q", verify.Detect(dir))
		}
		if !NeedsBootstrap(dir) {
			t.Error("a git repo with only a vacuous verifier must trigger bootstrap")
		}
	})

	t.Run("a git repo with a real verifier does NOT trigger", func(t *testing.T) {
		// A go.mod gives Detect a real, red-capable command → not greenfield.
		dir := gitRepoWith(t, map[string]string{"go.mod": "module x\n\ngo 1.22\n"})
		if verify.Detect(dir) == "true" {
			t.Fatal("precondition: a go.mod repo should have a real verifier")
		}
		if NeedsBootstrap(dir) {
			t.Error("a repo with a real verifier must NOT trigger bootstrap")
		}
	})
}

// --- Bootstrap: inited repo with a HEAD + a real (non-"true") verify command --

// On an EMPTY directory, Bootstrap inits a repo, makes it have a HEAD (so the
// first worktree can branch off it), and — when a scaffold seam writes a red
// verifier — chooses a verify command that is meaningful (not the vacuous "true").
func TestBootstrap_EmptyDir_HeadAndRedVerifier(t *testing.T) {
	dir := t.TempDir() // empty, not a git repo
	lg := tmpLog(t)

	// Advisor maps the goal → stack go + a verify command. The scaffold writes a
	// minimal go module + a FAILING test, so `go test ./...` (and our chosen cmd)
	// is genuinely RED on the skeleton.
	adv := advisor.New(replyModel{reply: "stack :: go\nverify :: go build ./... && go test ./..."}, 0)

	var scaffolded backend.Task
	scaffold := func(_ context.Context, task backend.Task) (backend.Result, error) {
		scaffolded = task
		// Simulate what a sandboxed native loop would write: a skeleton + a RED test.
		writeFile(t, task.Dir, "go.mod", "module svc\n\ngo 1.22\n")
		writeFile(t, task.Dir, "health_test.go",
			"package svc\n\nimport \"testing\"\n\nfunc TestHealth(t *testing.T){ t.Fatal(\"not implemented\") }\n")
		return backend.Result{Backend: "native", Summary: "scaffolded", SelfClaimed: true}, nil
	}

	res, err := Bootstrap(context.Background(), BootstrapConfig{
		Repo: dir, Goal: "build an HTTP health service", Advisor: adv, Scaffold: scaffold, Log: lg,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// 1) The repo now exists, is a git repo, and has a HEAD.
	if !isGitRepo(dir) {
		t.Fatal("Bootstrap did not init a git repo")
	}
	if !hasHeadTest(t, dir) {
		t.Fatal("Bootstrap left the repo without a HEAD (worktrees would crash)")
	}
	if res.HeadSHA == "" {
		t.Error("result HeadSHA is empty")
	}

	// 2) The chosen verify command is meaningful, not the vacuous "true".
	if res.VerifyCmd == "" || res.VerifyCmd == "true" {
		t.Errorf("VerifyCmd = %q, want a real (non-vacuous) command", res.VerifyCmd)
	}

	// 3) The scaffold ran with a goal that fences the project goal as DATA and was
	//    told to produce a RED verifier before any feature code.
	if scaffolded.Dir != dir {
		t.Errorf("scaffold ran in %q, want %q", scaffolded.Dir, dir)
	}
	if !strings.Contains(scaffolded.Goal, "RED") && !strings.Contains(scaffolded.Goal, "FAILS") {
		t.Errorf("scaffold goal did not demand a red verifier: %q", scaffolded.Goal)
	}
	if !strings.Contains(scaffolded.Goal, "BEGIN UNTRUSTED") {
		t.Errorf("scaffold goal did not fence the project goal as data: %q", scaffolded.Goal)
	}

	// 4) The scaffold's files were committed on top of the initial empty commit.
	if !res.Committed {
		t.Error("scaffold output was not committed")
	}
	if status := strings.TrimSpace(hgit(t, dir, "status", "--porcelain")); status != "" {
		t.Errorf("repo not clean after bootstrap commit:\n%s", status)
	}
	// Two commits: the initial empty one + the scaffold one.
	if n := strings.Count(hgit(t, dir, "log", "--oneline"), "\n"); n != 2 {
		t.Errorf("expected 2 commits (empty + scaffold), got %d lines", n)
	}
}

// With Repo=="" Bootstrap mints a fresh greenfield directory and returns its path,
// inited with a HEAD — the from-scratch path.
func TestBootstrap_EmptyRepoPath_MintsDir(t *testing.T) {
	res, err := Bootstrap(context.Background(), BootstrapConfig{Repo: "", Goal: "g", Log: tmpLog(t)})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(res.Repo) })
	if res.Repo == "" {
		t.Fatal("Bootstrap did not mint a greenfield dir for an empty Repo")
	}
	if !isGitRepo(res.Repo) {
		t.Error("minted dir is not a git repo")
	}
	if !hasHeadTest(t, res.Repo) {
		t.Error("minted repo has no HEAD")
	}
}

// With NO scaffold seam wired, Bootstrap still inits a repo with a HEAD (the
// initial empty commit) and reports it — the loop then drives the first real slice.
func TestBootstrap_NoScaffold_StillHeadAndEmptyCommit(t *testing.T) {
	dir := t.TempDir()
	res, err := Bootstrap(context.Background(), BootstrapConfig{Repo: dir, Goal: "g", Log: tmpLog(t)})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !hasHeadTest(t, dir) {
		t.Fatal("no-scaffold bootstrap left no HEAD")
	}
	if res.Committed {
		t.Error("no scaffold → nothing should be reported committed")
	}
	if res.HeadSHA == "" {
		t.Error("no-scaffold bootstrap reported no HEAD SHA")
	}
}

// A harness fault from the scaffold seam is surfaced as an error (not swallowed),
// but the repo still has its HEAD from the initial empty commit.
func TestBootstrap_ScaffoldFaultSurfaces(t *testing.T) {
	dir := t.TempDir()
	scaffold := func(context.Context, backend.Task) (backend.Result, error) {
		return backend.Result{}, errors.New("sandbox unavailable")
	}
	_, err := Bootstrap(context.Background(), BootstrapConfig{Repo: dir, Goal: "g", Scaffold: scaffold, Log: tmpLog(t)})
	if err == nil {
		t.Fatal("a scaffold harness fault must surface as an error")
	}
	if !hasHeadTest(t, dir) {
		t.Error("repo lost its HEAD on a scaffold fault")
	}
}

// The Override pins the VerifyCmd regardless of advice or detection.
func TestBootstrap_OverrideWins(t *testing.T) {
	dir := t.TempDir()
	adv := advisor.New(replyModel{reply: "stack :: go\nverify :: go test ./..."}, 0)
	res, err := Bootstrap(context.Background(), BootstrapConfig{
		Repo: dir, Goal: "g", Advisor: adv, Override: "make verify", Log: tmpLog(t),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.VerifyCmd != "make verify" {
		t.Errorf("Override ignored: VerifyCmd = %q, want %q", res.VerifyCmd, "make verify")
	}
}

// --- PromotionPermitted: the vacuous-verifier predicate (design risk #6) ------

func TestPromotionPermitted(t *testing.T) {
	tests := []struct {
		name     string
		verifier verify.Verifier
		want     bool
	}{
		{
			name:     "currently RED verifier → promotion permitted (the check is real)",
			verifier: &fixedVerifier{pass: false},
			want:     true,
		},
		{
			name:     "currently GREEN on greenfield → vacuous → promotion REFUSED",
			verifier: &fixedVerifier{pass: true},
			want:     false,
		},
		{
			name:     "nil verifier (no check at all) → REFUSED",
			verifier: nil,
			want:     false,
		},
		{
			name:     "verifier transport error → unestablished → REFUSED",
			verifier: &fixedVerifier{err: errors.New("boom")},
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromotionPermitted(context.Background(), tc.verifier); got != tc.want {
				t.Errorf("PromotionPermitted = %t, want %t", got, tc.want)
			}
		})
	}
}

// The promotion predicate is what makes "no checks ⇒ everything looks done"
// non-fatal: a vacuous green verifier on a feature-less tree can NEVER promote,
// while a red verifier (which only goes green once feature code lands) can. We
// drive both directions through the real verify.CommandVerifier over a scriptBox.
func TestPromotionPermitted_VacuousVsReal(t *testing.T) {
	// Vacuous: `true` always exits 0 → green-right-now → refuse.
	vacuous := verify.New(&scriptBox{def: sandbox.Result{ExitCode: 0}}, "true")
	if PromotionPermitted(context.Background(), vacuous) {
		t.Error("a vacuous always-green verifier must NOT permit promotion")
	}
	// Real: a command that exits non-zero on the empty skeleton → red → permit.
	realRed := verify.New(&scriptBox{def: sandbox.Result{ExitCode: 1}}, "go test ./...")
	if !PromotionPermitted(context.Background(), realRed) {
		t.Error("a currently-red verifier must permit promotion")
	}
}

// --- logging: project_bootstrap is metadata only, no secrets ------------------

// project_bootstrap is logged as METADATA only — never the goal, never command
// output, never a secret. We assert the event exists, carries the stack/committed
// metadata, and that the persisted line contains neither the goal text nor a
// scaffolded secret-looking value.
func TestBootstrap_LogsMetadataNoSecrets(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	const secretGoal = "build a thing using API key sk-supersecret-DEADBEEF"
	adv := advisor.New(replyModel{reply: "stack :: go\nverify :: go test ./..."}, 0)
	scaffold := func(_ context.Context, task backend.Task) (backend.Result, error) {
		writeFile(t, task.Dir, "go.mod", "module x\n\ngo 1.22\n")
		return backend.Result{}, nil
	}
	if _, err := Bootstrap(context.Background(), BootstrapConfig{
		Repo: dir, Goal: secretGoal, Advisor: adv, Scaffold: scaffold, Log: lg,
	}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	_ = lg.Close()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	body := string(raw)

	// The event must be present with its metadata.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		var ev eventlog.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if ev.Kind == "project_bootstrap" {
			found = true
			if ev.Detail["stack"] != "go" {
				t.Errorf("project_bootstrap stack metadata = %v, want go", ev.Detail["stack"])
			}
			if ev.Detail["verify_chosen"] != true {
				t.Errorf("project_bootstrap verify_chosen = %v, want true", ev.Detail["verify_chosen"])
			}
		}
	}
	if !found {
		t.Fatal("no project_bootstrap event was logged")
	}

	// The goal text (and the secret inside it) must NEVER appear in the log.
	if strings.Contains(body, "supersecret") || strings.Contains(body, secretGoal) {
		t.Error("the goal text / a secret leaked into the bootstrap log")
	}
	if strings.Contains(body, "sk-supersecret-DEADBEEF") {
		t.Error("a secret-looking token leaked into the bootstrap log")
	}
}

// --- small fakes/helpers local to this test file -----------------------------

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
