package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/budget"
	"nilcore/internal/model"
	"nilcore/internal/store"
	"nilcore/internal/super"
	"nilcore/internal/worktree"
)

// TestResumeRefSanitization: the durable pin lives under resume/ (never swept) and the
// task id's ':' is mapped to '-' (git refs forbid ':').
func TestResumeRefSanitization(t *testing.T) {
	if got := superviseTaskID("999-3"); got != "supervise:999-3" {
		t.Errorf("superviseTaskID = %q", got)
	}
	if got := resumeRef("supervise:999-3"); got != "resume/supervise-999-3" {
		t.Errorf("resumeRef = %q, want resume/supervise-999-3 (no colon)", got)
	}
	if strings.ContainsRune(resumeRef("supervise:abc"), ':') {
		t.Error("a resume ref must not contain ':' (invalid git ref)")
	}
}

// TestSnapshotTranslationRoundTrip: the leaf super.Snapshot ⇄ durable agent.RunState
// translation is field-for-field, so a checkpointed snapshot reseeds losslessly.
func TestSnapshotTranslationRoundTrip(t *testing.T) {
	snap := super.Snapshot{TipSHA: "deadbeef", Nodes: []super.SnapNode{
		{ID: "t1", State: "merged"},
		{ID: "t2", DependsOn: []string{"t1"}, State: "pending"},
	}}
	rs := translateSnapshot(snap)
	if rs.TipSHA != "deadbeef" || len(rs.Nodes) != 2 || rs.Nodes[0].State != agent.NodeMerged {
		t.Fatalf("translateSnapshot wrong: %+v", rs)
	}
	seed := resumeStateFrom("supervise:t", rs)
	if seed.TipSHA != "deadbeef" || seed.TipBranch != "resume/supervise-t" || len(seed.Nodes) != 2 {
		t.Fatalf("resumeStateFrom wrong: %+v", seed)
	}
	if seed.Nodes[1].ID != "t2" || seed.Nodes[1].State != "pending" || len(seed.Nodes[1].DependsOn) != 1 {
		t.Errorf("resume node lost fields: %+v", seed.Nodes[1])
	}
}

// TestSuperviseResumeWiringEndToEnd is the PR-2 gate over real git + a real store: it
// drives the wired SaveState seam (as a doIntegrate would), then replays exactly as a
// restart does, asserting the load-bearing properties:
//   - SaveState pins the verified tip under resume/<taskID> AND records the run under
//     SuperviseStatus (not "running" — so the native resume pass never grabs it).
//   - The pinned tip survives the run-end branch sweep (cleanup), keeping the merged
//     work reachable — the durable-resume blocker fix.
//   - On restart the snapshot loads back; ResumePlan = Merged[t1] / Release[t2] (no
//     double-merge of t1, t2 re-released); the rebuilt stack is SEEDED from the tip.
//   - Complete flips the row to done and drops the pin.
func TestSuperviseResumeWiringEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repo := newGoRepo(t)
	log := openTestLog(t)

	// A real "verified integration tip": main + t1's merged work, captured as a SHA the
	// way doIntegrate's last MergeResult would report it.
	gitT := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitT("checkout", "-q", "-b", "task/super.t1", "main")
	if err := os.WriteFile(filepath.Join(repo, "t1.txt"), []byte("from t1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT("add", "-A")
	gitT("commit", "-q", "-m", "t1 work")
	gitT("checkout", "-q", "-b", "integrate/seq", "main")
	gitT("merge", "--no-ff", "-q", "-m", "integrate t1", "task/super.t1")
	tipSHA := gitT("rev-parse", "HEAD")
	gitT("checkout", "-q", "main") // the live integration worktree is thrown away by a crash

	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cp := agent.NewCheckpoint(st)
	taskID := superviseTaskID("thread-7")
	prov := &fakeProvider{id: "claude-opus-4-8", usage: model.Usage{OutputTokens: 10}}

	deps := buildDeps{
		goal: "build the service", dir: repo, runtime: "podman", image: defaultBuildImage,
		maxIter: 12, maxFan: 8, maxAgent: 64, maxDepth: 1, maxSteps: 80,
		executor: prov, strong: prov, log: log, approver: denyApprover{},
		ledger: budget.New(), checkpoint: cp, taskID: taskID,
	}
	stack, err := buildStack(deps)
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}
	if stack.sup.SaveState == nil {
		t.Fatal("buildStack did not wire SaveState when a checkpoint + taskID were supplied")
	}

	// Drive the seam exactly as doIntegrate does after merging t1 (t2 spawned, pending).
	snap := super.Snapshot{TipSHA: tipSHA, Nodes: []super.SnapNode{
		{ID: "t1", State: "merged"},
		{ID: "t2", DependsOn: []string{"t1"}, State: "pending"},
	}}
	if err := stack.sup.SaveState(ctx, snap); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// The run row is "supervise" (NOT running) — the native resume pass ignores it.
	if got, _ := st.GetTask(ctx, taskID); got.Status != agent.SuperviseStatus {
		t.Fatalf("run row status = %q, want %q", got.Status, agent.SuperviseStatus)
	}
	if in, _ := cp.InFlight(ctx); len(in) != 0 {
		t.Fatalf("native InFlight must not see the supervise row, got %+v", in)
	}
	// The tip is pinned under resume/.
	if got := gitT("rev-parse", resumeRef(taskID)); got != tipSHA {
		t.Fatalf("tip not pinned: resume ref = %q, want %q", got, tipSHA)
	}

	// The run-end branch sweep reclaims the throwaway prefixes; the resume pin survives.
	stack.cleanup()
	for _, p := range []string{"task/", "rebase/", "integrate/", "read/"} {
		worktree.DeleteBranches(ctx, repo, p)
	}
	if got := gitT("rev-parse", resumeRef(taskID)); got != tipSHA {
		t.Fatalf("resume pin did NOT survive the run-end sweep: %q want %q", got, tipSHA)
	}

	// --- restart: load the snapshot; it survived verbatim. ---
	loaded, err := cp.LoadRunState(ctx, taskID)
	if err != nil || loaded.TipSHA != tipSHA {
		t.Fatalf("snapshot not durable: %+v err=%v", loaded, err)
	}
	plan := loaded.ResumePlan()
	if len(plan.Merged) != 1 || plan.Merged[0] != "t1" {
		t.Errorf("ResumePlan.Merged = %v, want [t1] (no double-merge)", plan.Merged)
	}
	if len(plan.Release) != 1 || plan.Release[0] != "t2" {
		t.Errorf("ResumePlan.Release = %v, want [t2] (re-release only the un-merged node)", plan.Release)
	}
	// No work lost: the pinned tip still carries t1's merged file.
	wt, err := worktree.CreateFrom(ctx, repo, "verify-resume/x", "vr-x", tipSHA)
	if err != nil {
		t.Fatalf("rebuild worktree from pinned tip: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()
	if b, _ := os.ReadFile(filepath.Join(wt.Path(), "t1.txt")); strings.TrimSpace(string(b)) != "from t1" {
		t.Errorf("merged t1 work missing from rebuilt tip")
	}

	// The rebuilt stack is SEEDED from the tip (the supervisor would plan only t2).
	seed := resumeStateFrom(taskID, loaded)
	resumeDeps := deps
	resumeDeps.ledger = budget.New()
	resumeDeps.baseRef = tipSHA
	resumeDeps.resume = seed
	stack2, err := buildStack(resumeDeps)
	if err != nil {
		t.Fatalf("buildStack (resume): %v", err)
	}
	defer stack2.cleanup()
	if stack2.sup.Resume == nil || stack2.sup.Resume.TipSHA != tipSHA {
		t.Fatalf("resumed supervisor not seeded from the tip: %+v", stack2.sup.Resume)
	}
	if stack2.sup.Resume.TipBranch != resumeRef(taskID) {
		t.Errorf("resume seed TipBranch = %q, want %q", stack2.sup.Resume.TipBranch, resumeRef(taskID))
	}

	// Complete flips the row to done and drops the pin (finalize on a verified-done run).
	finalizeSupervise(ctx, serveDeps{checkpoint: cp, baseRepo: repo, log: log}, taskID, "build the service", true)
	if got, _ := st.GetTask(ctx, taskID); got.Status != "done" {
		t.Errorf("after finalize, status = %q, want done", got.Status)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", resumeRef(taskID)).CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("resume pin not dropped on done: %q", out)
	}
}
