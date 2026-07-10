package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/store"
	"nilcore/internal/worktree"
)

// reattachBackend records the worktree HEAD it is handed at Run time, so a test can
// prove whether the drive was based on a preserved suspend ref (== the ref's commit)
// or a fresh HEAD worktree (== the repo HEAD). It never writes — the verifier passes.
type reattachBackend struct {
	headAtRun string
	ran       bool
}

func (b *reattachBackend) Name() string { return "reattach" }
func (b *reattachBackend) Run(_ context.Context, t backend.Task) (backend.Result, error) {
	b.ran = true
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = t.Dir
	out, _ := cmd.CombinedOutput()
	b.headAtRun = strings.TrimSpace(string(out))
	return backend.Result{Backend: "reattach", Summary: "ran"}, nil
}

// seedSuspendBranch creates ref in repo pointing at a NEW commit (marker file) that is
// NOT reachable from HEAD — mimicking a suspended drive's preserved committed work. It
// returns that commit's SHA. The temp worktree used to build it is removed (the branch
// is kept), so ref is a bare recovery anchor exactly like a real suspend/<id>.
func seedSuspendBranch(t *testing.T, repo, ref, marker string) string {
	t.Helper()
	wtDir := filepath.Join(t.TempDir(), "seedwt")
	gitTrim(t, repo, "worktree", "add", "-b", ref, wtDir, "HEAD")
	if err := os.WriteFile(filepath.Join(wtDir, marker), []byte("preserved before sleep\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	gitTrim(t, wtDir, "add", "-A")
	gitTrim(t, wtDir, "-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "-q", "-m", "C1 preserved work")
	sha := gitTrim(t, wtDir, "rev-parse", "HEAD")
	gitTrim(t, repo, "worktree", "remove", "--force", wtDir)
	return sha
}

func refExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	return gitTrim(t, repo, "rev-parse", "--verify", "--quiet", ref) != ""
}

// --- FIX 1: ResumeBranch unit behavior (prefix match, cross-prefix, most-recent, nil) ---

func TestResumeBranch(t *testing.T) {
	ctx := context.Background()

	// nil receiver is a clean no-op (an orchestrator with no checkpoint).
	var nilC *agent.Checkpoint
	if b, id, err := nilC.ResumeBranch(ctx, "conv-4"); b != "" || id != "" || err != nil {
		t.Errorf("nil receiver = (%q,%q,%v), want ('','',nil)", b, id, err)
	}

	c, _ := newCheckpoint(t)
	if err := c.Suspend(ctx, "conv-3", "g", "suspend/conv-3"); err != nil {
		t.Fatal(err)
	}
	// A resuming sibling in the same conversation reattaches onto conv-3's ref.
	if b, id, err := c.ResumeBranch(ctx, "conv-4"); err != nil || b != "suspend/conv-3" || id != "conv-3" {
		t.Errorf("prefix match = (%q,%q,%v), want ('suspend/conv-3','conv-3',nil)", b, id, err)
	}
	// A different conversation must NOT match.
	if b, id, _ := c.ResumeBranch(ctx, "other-4"); b != "" || id != "" {
		t.Errorf("cross-prefix = (%q,%q), want empty (a different conversation must not correlate)", b, id)
	}
	// A re-driven id must not reattach onto its OWN suspend row.
	if b, _, _ := c.ResumeBranch(ctx, "conv-3"); b != "" {
		t.Errorf("own-id reattach = %q, want empty", b)
	}
}

func TestResumeBranchMostRecent(t *testing.T) {
	ctx := context.Background()
	c, _ := newCheckpoint(t)
	// Two suspended siblings; id-ascending order makes conv-5 the more recent.
	if err := c.Suspend(ctx, "conv-2", "g", "suspend/conv-2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Suspend(ctx, "conv-5", "g", "suspend/conv-5"); err != nil {
		t.Fatal(err)
	}
	b, id, err := c.ResumeBranch(ctx, "conv-9")
	if err != nil {
		t.Fatal(err)
	}
	if b != "suspend/conv-5" || id != "conv-5" {
		t.Errorf("most-recent = (%q,%q), want the latest sibling ('suspend/conv-5','conv-5')", b, id)
	}
}

func TestResumeBranchSkipsEmptyBranch(t *testing.T) {
	ctx := context.Background()
	c, _ := newCheckpoint(t)
	// A suspended sibling whose preserved branch is empty (nothing was committed) is
	// not reattachable and must be ignored.
	if err := c.Suspend(ctx, "conv-3", "g", ""); err != nil {
		t.Fatal(err)
	}
	if b, id, _ := c.ResumeBranch(ctx, "conv-4"); b != "" || id != "" {
		t.Errorf("empty-branch sibling = (%q,%q), want empty", b, id)
	}
}

// --- FIX 1: executeSingle auto-reattach, fallback, and unchanged default ---

// A resumed drive whose session predecessor preserved committed work is based on that
// work (the suspend ref), the predecessor is retired, the consumed ref is deleted, and
// a task_resumed correlation event is logged.
func TestExecuteReattachesToSuspendRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	conv := "conv-reattach"
	suspendedID := conv + "-3"
	resumeID := conv + "-4"
	ref := "suspend/" + suspendedID
	c1 := seedSuspendBranch(t, repo, ref, "resumed.txt")

	ckpt, s := newCheckpoint(t)
	ctx := context.Background()
	if err := ckpt.Suspend(ctx, suspendedID, "nap goal", ref); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "events.log")
	lg, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	be := &reattachBackend{}
	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		Log:        lg,
		NewEnv:     func(string) agent.Env { return agent.Env{Backend: be, Verifier: &fakeVerifier{passed: true}} },
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Checkpoint: ckpt,
	}
	if _, err := orch.Execute(ctx, backend.Task{ID: resumeID, Goal: "resume goal"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = lg.Close()

	if !be.ran {
		t.Fatal("backend did not run")
	}
	if be.headAtRun != c1 {
		t.Errorf("resumed worktree HEAD = %q, want C1 %q (must be based on the suspend ref, not HEAD)", be.headAtRun, c1)
	}
	// The predecessor is retired so it is never reattached twice.
	rec, gerr := s.GetTask(ctx, suspendedID)
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if rec.Status == "suspended" {
		t.Error("suspended predecessor still 'suspended' — it must be retired after a reattach")
	}
	// The consumed anchor is deleted immediately.
	if refExists(t, repo, ref) {
		t.Errorf("consumed suspend ref %q still present — must be deleted after reattach", ref)
	}
	// The correlation is auditable.
	found := false
	for _, e := range readEvents(t, logPath) {
		if e.Kind == "task_resumed" {
			found = true
			if e.Detail["resumed_from"] != suspendedID {
				t.Errorf("task_resumed.resumed_from = %v, want %q", e.Detail["resumed_from"], suspendedID)
			}
			if e.Detail["branch"] != ref {
				t.Errorf("task_resumed.branch = %v, want %q", e.Detail["branch"], ref)
			}
		}
	}
	if !found {
		t.Error("no task_resumed event was logged")
	}
}

// A recorded predecessor whose suspend ref is GONE (stale/swept) must fall back to a
// fresh HEAD worktree — no error, no data loss — and leave the row for a later sweep.
func TestExecuteFallsBackWhenSuspendRefMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	conv := "conv-fallback"
	suspendedID := conv + "-3"
	resumeID := conv + "-4"
	ref := "suspend/" + suspendedID // recorded, but never created as a real ref

	ckpt, s := newCheckpoint(t)
	ctx := context.Background()
	if err := ckpt.Suspend(ctx, suspendedID, "nap goal", ref); err != nil {
		t.Fatal(err)
	}

	be := &reattachBackend{}
	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(string) agent.Env { return agent.Env{Backend: be, Verifier: &fakeVerifier{passed: true}} },
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Checkpoint: ckpt,
	}
	if _, err := orch.Execute(ctx, backend.Task{ID: resumeID, Goal: "resume goal"}); err != nil {
		t.Fatalf("Execute must fall back cleanly, got: %v", err)
	}
	if !be.ran {
		t.Error("backend did not run on the fallback path")
	}
	repoHead := gitTrim(t, repo, "rev-parse", "HEAD")
	if be.headAtRun != repoHead {
		t.Errorf("fallback worktree HEAD = %q, want repo HEAD %q (a stale ref must fall back to HEAD)", be.headAtRun, repoHead)
	}
	// The predecessor is NOT retired on a fallback — its row waits for a later sweep.
	rec, _ := s.GetTask(ctx, suspendedID)
	if rec.Status != "suspended" {
		t.Errorf("on fallback the predecessor row must stay 'suspended', got %q", rec.Status)
	}
}

// No suspended predecessor ⇒ the worktree is created off HEAD exactly as before.
func TestExecuteDefaultOffHeadWhenNoPredecessor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ckpt, _ := newCheckpoint(t) // empty store: no suspended rows
	ctx := context.Background()

	be := &reattachBackend{}
	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(string) agent.Env { return agent.Env{Backend: be, Verifier: &fakeVerifier{passed: true}} },
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Checkpoint: ckpt,
	}
	if _, err := orch.Execute(ctx, backend.Task{ID: "conv-solo-1", Goal: "x"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	repoHead := gitTrim(t, repo, "rev-parse", "HEAD")
	if be.headAtRun != repoHead {
		t.Errorf("default worktree HEAD = %q, want repo HEAD %q (no predecessor ⇒ off HEAD)", be.headAtRun, repoHead)
	}
}

// --- FIX 2: SweepSuspended reclaims resolved + oldest-beyond-keep anchors ---

func TestSweepSuspended(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ckpt, s := newCheckpoint(t)
	ctx := context.Background()

	head := gitTrim(t, repo, "rev-parse", "HEAD")
	refs := []string{"suspend/conv-1", "suspend/conv-2", "suspend/conv-3", "suspend/conv-4", "suspend/gone-9"}
	for _, r := range refs {
		if err := worktree.PinBranch(ctx, repo, r, head); err != nil {
			t.Fatalf("PinBranch %s: %v", r, err)
		}
	}

	// Row states:
	//  - conv-1: resolved (status "failed") → dead anchor, must be swept.
	//  - conv-2, conv-3, conv-4: still "suspended".
	//  - gone-9: NO row at all → orphan anchor, must be swept.
	_ = s.UpsertTask(ctx, store.Task{ID: "conv-1", Goal: "g", Status: "failed", Detail: `{"branch":"suspend/conv-1"}`})
	for _, id := range []string{"conv-2", "conv-3", "conv-4"} {
		if err := ckpt.Suspend(ctx, id, "g", "suspend/"+id); err != nil {
			t.Fatal(err)
		}
	}

	// keep=2 → of the three still-suspended, keep the 2 most-recent (conv-3, conv-4)
	// and delete the oldest (conv-2); plus delete conv-1 (resolved) and gone-9 (no row).
	if err := ckpt.SweepSuspended(ctx, repo, 2); err != nil {
		t.Fatalf("SweepSuspended: %v", err)
	}

	if refExists(t, repo, "suspend/conv-1") {
		t.Error("resolved anchor suspend/conv-1 must be swept")
	}
	if refExists(t, repo, "suspend/gone-9") {
		t.Error("orphan anchor suspend/gone-9 (no row) must be swept")
	}
	if refExists(t, repo, "suspend/conv-2") {
		t.Error("oldest still-suspended anchor beyond keep must be swept")
	}
	if !refExists(t, repo, "suspend/conv-3") {
		t.Error("still-suspended anchor within keep must survive")
	}
	if !refExists(t, repo, "suspend/conv-4") {
		t.Error("most-recent still-suspended anchor must survive")
	}

	// Idempotent: a second sweep over the survivors is a clean no-op (no panic/error).
	if err := ckpt.SweepSuspended(ctx, repo, 2); err != nil {
		t.Fatalf("second SweepSuspended: %v", err)
	}
	if !refExists(t, repo, "suspend/conv-4") {
		t.Error("a second sweep must not drop a still-kept anchor")
	}
}

// SweepSuspended over a repo with no suspend/ refs is a clean no-op.
func TestSweepSuspendedNoRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ckpt, _ := newCheckpoint(t)
	if err := ckpt.SweepSuspended(context.Background(), repo, 0); err != nil {
		t.Fatalf("SweepSuspended over empty repo: %v", err)
	}
	// nil receiver is also a clean no-op.
	var nilC *agent.Checkpoint
	if err := nilC.SweepSuspended(context.Background(), repo, 0); err != nil {
		t.Fatalf("nil-receiver SweepSuspended: %v", err)
	}
}
