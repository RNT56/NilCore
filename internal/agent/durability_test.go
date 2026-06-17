package agent_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/store"
	"nilcore/internal/worktree"
)

func newCheckpoint(t *testing.T) (*agent.Checkpoint, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return agent.NewCheckpoint(s), s
}

// SaveWake/LoadWakes/DisarmWake round-trip over the real store; a fired (disarmed)
// wake is excluded; and the wake rows never cross-contaminate the conversation/
// running statuses the resume path reads.
func TestWakeRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, s := newCheckpoint(t)

	if err := c.SaveWake(ctx, "thread-A", `{"note":"check CI"}`); err != nil {
		t.Fatalf("SaveWake: %v", err)
	}
	if err := c.SaveWake(ctx, "thread-B", `{"note":"deploy"}`); err != nil {
		t.Fatalf("SaveWake: %v", err)
	}
	// A conversation row + a running task must NOT appear among wakes.
	if err := c.SaveConversation(ctx, "thread-A", "goal", `{"x":1}`); err != nil {
		t.Fatal(err) // same threadID, different status — must not collide with the wake row
	}
	_ = s.UpsertTask(ctx, store.Task{ID: "job-1", Goal: "g", Status: "running"})

	wakes, err := c.LoadWakes(ctx)
	if err != nil {
		t.Fatalf("LoadWakes: %v", err)
	}
	if len(wakes) != 2 || wakes["thread-A"] != `{"note":"check CI"}` || wakes["thread-B"] != `{"note":"deploy"}` {
		t.Fatalf("LoadWakes = %v, want exactly the two armed wakes (no conversation/running rows)", wakes)
	}

	// Disarm thread-A → excluded; thread-B survives (durable across a fresh Checkpoint).
	if err := c.DisarmWake(ctx, "thread-A"); err != nil {
		t.Fatalf("DisarmWake: %v", err)
	}
	fresh := agent.NewCheckpoint(s)
	wakes2, _ := fresh.LoadWakes(ctx)
	if len(wakes2) != 1 || wakes2["thread-B"] == "" {
		t.Errorf("after Disarm(A) + restart, want only thread-B armed, got %v", wakes2)
	}
	// Disarm with nothing armed is a clean no-op.
	if err := c.DisarmWake(ctx, "no-such-thread"); err != nil {
		t.Errorf("Disarm of an unarmed thread should be a no-op, got %v", err)
	}
}

func TestSIGTERMCheckpointAndResume(t *testing.T) {
	cp, s := newCheckpoint(t)
	ctx := context.Background()

	// A task begins...
	if err := cp.Begin(ctx, backend.Task{ID: "t1", Goal: "fix the bug"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Status != "running" {
		t.Fatalf("status = %q, want running", got.Status)
	}

	// SIGTERM: checkpoint cleanly (no partial state).
	if err := cp.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Status != "interrupted" {
		t.Fatalf("after interrupt status = %q, want interrupted", got.Status)
	}

	// Restart: the in-flight task is resumed from its checkpoint.
	inflight, _ := cp.InFlight(ctx)
	if len(inflight) != 1 || inflight[0].ID != "t1" {
		t.Fatalf("InFlight = %+v, want [t1]", inflight)
	}
	var resumed string
	err := cp.Resume(ctx, func(_ context.Context, tk backend.Task) (bool, error) {
		resumed = tk.ID
		return true, nil // succeeds on resume
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed != "t1" {
		t.Errorf("resumed %q, want t1", resumed)
	}
	if got, _ := s.GetTask(ctx, "t1"); got.Status != "done" {
		t.Errorf("after resume status = %q, want done", got.Status)
	}
}

func TestResumeFailsCleanly(t *testing.T) {
	cp, s := newCheckpoint(t)
	ctx := context.Background()
	_ = cp.Begin(ctx, backend.Task{ID: "t2", Goal: "x"})

	err := cp.Resume(ctx, func(context.Context, backend.Task) (bool, error) {
		return false, errors.New("backend exploded") // resume fails
	})
	if err != nil {
		t.Fatalf("Resume should not propagate per-task errors: %v", err)
	}
	if got, _ := s.GetTask(ctx, "t2"); got.Status != "failed" {
		t.Errorf("a task that can't resume must be failed cleanly; status=%q", got.Status)
	}
}

// --- P5-T03 multi-agent run-state durability ---

func TestRunStateMarshalRoundTrip(t *testing.T) {
	rs := agent.RunState{
		TipSHA: "deadbeef",
		Nodes: []agent.Node{
			{ID: "t1", Branch: "task/super.t1", State: agent.NodeMerged},
			{ID: "t2", Branch: "task/super.t2", DependsOn: []string{"t1"}, State: agent.NodePending},
		},
	}
	blob, err := rs.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := agent.UnmarshalRunState(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rs) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, rs)
	}
	// An empty blob is a valid empty snapshot (single task / pre-integration), not
	// an error.
	if z, err := agent.UnmarshalRunState(""); err != nil || z.TipSHA != "" || len(z.Nodes) != 0 {
		t.Errorf("empty detail: got %+v, %v", z, err)
	}
}

// TestResumePlanPartition is the no-double-merge / no-work-lost logic in isolation:
// merged nodes are replayed (never re-merged), un-merged nodes are re-released ONLY
// when every dependency is already merged, and a node blocked by a non-merged dep
// waits (it is not re-released this pass).
func TestResumePlanPartition(t *testing.T) {
	tests := []struct {
		name    string
		rs      agent.RunState
		merged  []string
		release []string
		skip    []string
	}{
		{
			name: "tip-built-with-some-merged",
			rs: agent.RunState{
				TipSHA: "tipsha",
				Nodes: []agent.Node{
					{ID: "t1", State: agent.NodeMerged},
					{ID: "t2", DependsOn: []string{"t1"}, State: agent.NodePending}, // dep merged → ready
					{ID: "t3", DependsOn: []string{"t2"}, State: agent.NodePending}, // dep not merged → wait
				},
			},
			merged:  []string{"t1"},
			release: []string{"t2"},
			skip:    []string{"t3"},
		},
		{
			name: "failed-node-is-re-released-when-deps-merged",
			rs: agent.RunState{Nodes: []agent.Node{
				{ID: "a", State: agent.NodeMerged},
				{ID: "b", DependsOn: []string{"a"}, State: agent.NodeFailed}, // retry off rebuilt tip
			}},
			merged:  []string{"a"},
			release: []string{"b"},
		},
		{
			name: "skipped-node-stays-skipped",
			rs: agent.RunState{Nodes: []agent.Node{
				{ID: "x", State: agent.NodeFailed},
				{ID: "y", DependsOn: []string{"x"}, State: agent.NodeSkipped},
			}},
			release: []string{"x"}, // no deps → ready to retry
			skip:    []string{"y"}, // terminal skip
		},
		{
			name: "root-pending-with-no-deps-is-released",
			rs: agent.RunState{Nodes: []agent.Node{
				{ID: "only", State: agent.NodePending},
			}},
			release: []string{"only"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := tc.rs.ResumePlan()
			if plan.TipSHA != tc.rs.TipSHA {
				t.Errorf("TipSHA = %q, want %q", plan.TipSHA, tc.rs.TipSHA)
			}
			assertSet(t, "Merged", plan.Merged, tc.merged)
			assertSet(t, "Release", plan.Release, tc.release)
			assertSet(t, "Skip", plan.Skip, tc.skip)
			// Every node lands in exactly one bucket (partition invariant).
			total := len(plan.Merged) + len(plan.Release) + len(plan.Skip)
			if total != len(tc.rs.Nodes) {
				t.Errorf("partition covers %d nodes, want %d", total, len(tc.rs.Nodes))
			}
		})
	}
}

func assertSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) == 0 {
		g = nil
	}
	if len(w) == 0 {
		w = nil
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

// TestSIGTERMMidRunReplayFromTip is the P5-T03 acceptance test end-to-end over a
// real git repo. A multi-agent run merges subagent t1 into the integration tip,
// gets SIGTERM'd before t2 is integrated, persists its snapshot (tip SHA +
// per-node state), and on restart:
//   - rebuilds the integration worktree from the LOGGED tip SHA (t1's merged work
//     is present — no work lost), and
//   - re-releases ONLY t2 (t1 is never re-merged — no double-merge).
func TestSIGTERMMidRunReplayFromTip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	cp, _ := newCheckpoint(t)

	// A base repo with an initial commit, plus a branch carrying subagent t1's work.
	repo := t.TempDir()
	gitInit(t, repo)
	writeCommit(t, repo, "base.txt", "base", "init")
	git(t, repo, "checkout", "-q", "-b", "task/super.t1")
	writeCommit(t, repo, "t1.txt", "from t1", "t1 work")
	git(t, repo, "checkout", "-q", "master")
	// (a t2 branch would exist too, but it never gets merged before the crash.)

	// --- mid-run: the integrator merged t1 into an integration tip and re-verified
	// it green. We model that tip as a real merge commit on the base repo, then take
	// its SHA as the durable integration tip the snapshot pins. ---
	git(t, repo, "checkout", "-q", "-b", "integration")
	git(t, repo, "-c", "user.email=t@x", "-c", "user.name=t", "merge", "--no-ff", "-q", "-m", "integrate t1", "task/super.t1")
	tipSHA := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
	git(t, repo, "checkout", "-q", "master") // the live worktree was thrown away by the crash

	// Begin + snapshot, then SIGTERM (Interrupt). Snapshot MUST survive the interrupt.
	if err := cp.Begin(ctx, backend.Task{ID: "run", Goal: "build the service"}); err != nil {
		t.Fatal(err)
	}
	snap := agent.RunState{
		TipSHA: tipSHA,
		Nodes: []agent.Node{
			{ID: "t1", Branch: "task/super.t1", State: agent.NodeMerged},
			{ID: "t2", Branch: "task/super.t2", DependsOn: []string{"t1"}, State: agent.NodePending},
		},
	}
	if err := cp.SaveRunState(ctx, "run", "build the service", snap); err != nil {
		t.Fatal(err)
	}
	if err := cp.Interrupt(ctx); err != nil { // SIGTERM checkpoint
		t.Fatal(err)
	}

	// --- restart: load the snapshot back; it must survive the interrupt verbatim. ---
	loaded, err := cp.LoadRunState(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TipSHA != tipSHA {
		t.Fatalf("tip SHA not durable across SIGTERM: got %q want %q", loaded.TipSHA, tipSHA)
	}
	plan := loaded.ResumePlan()

	// No double-merge: t1 is replayed, only t2 is re-released.
	assertSet(t, "Merged", plan.Merged, []string{"t1"})
	assertSet(t, "Release", plan.Release, []string{"t2"})

	// No work lost: rebuilding the integration worktree from the logged tip SHA
	// brings back t1's merged file — exactly the convergence the crash interrupted.
	wt, err := worktree.CreateFrom(ctx, repo, "resume/integration", "resume-int", plan.TipSHA)
	if err != nil {
		t.Fatalf("rebuild integration worktree from tip SHA: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	if got := readFile(t, wt.Path(), "t1.txt"); got != "from t1" {
		t.Errorf("merged t1 work missing from rebuilt tip: %q", got)
	}
	// And t2's work was never integrated (it crashed before t2 ran).
	if _, err := os.Stat(filepath.Join(wt.Path(), "t2.txt")); !os.IsNotExist(err) {
		t.Errorf("t2.txt present on rebuilt tip — t2 should NOT be merged (err=%v)", err)
	}
}

// TestSuperviseStatusIsolatesFromNativeResume is the regression guard for PR-2: a
// multi-agent run's snapshot is recorded under the DISTINCT SuperviseStatus, so the
// single-native resume path (InFlight → running/interrupted) never re-drives it as one
// native drive (which would orphan the integration tip and redo merged work). Only the
// dedicated InFlightSupervise pass sees it; the SIGTERM Interrupt sweep leaves it alone;
// and Complete flips it terminal so it is not resumed again.
func TestSuperviseStatusIsolatesFromNativeResume(t *testing.T) {
	ctx := context.Background()
	cp, s := newCheckpoint(t)

	// A live native task AND a multi-agent run are both in flight.
	if err := cp.Begin(ctx, backend.Task{ID: "native-1", Goal: "fix"}); err != nil {
		t.Fatal(err)
	}
	snap := agent.RunState{TipSHA: "tipsha", Nodes: []agent.Node{{ID: "t1", State: agent.NodeMerged}}}
	if err := cp.SaveRunState(ctx, "supervise:thread-9", "build it", snap); err != nil {
		t.Fatal(err)
	}

	// The supervise row is "supervise", not "running".
	if got, _ := s.GetTask(ctx, "supervise:thread-9"); got.Status != agent.SuperviseStatus {
		t.Fatalf("supervise row status = %q, want %q", got.Status, agent.SuperviseStatus)
	}

	// The native resume pass sees ONLY the native task — never the supervise row.
	inflight, err := cp.InFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inflight) != 1 || inflight[0].ID != "native-1" {
		t.Fatalf("InFlight = %+v, want only [native-1] (supervise row must NOT appear)", inflight)
	}

	// The dedicated supervise pass sees ONLY the supervise row, with its snapshot intact.
	sup, err := cp.InFlightSupervise(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sup) != 1 || sup[0].ID != "supervise:thread-9" {
		t.Fatalf("InFlightSupervise = %+v, want only [supervise:thread-9]", sup)
	}

	// SIGTERM checkpoint marks running→interrupted; the supervise row is untouched.
	if err := cp.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, "supervise:thread-9"); got.Status != agent.SuperviseStatus {
		t.Errorf("Interrupt clobbered the supervise row: status = %q, want %q", got.Status, agent.SuperviseStatus)
	}
	// And it survives a fresh Checkpoint (durable across restart) with its tip intact.
	loaded, err := agent.NewCheckpoint(s).LoadRunState(ctx, "supervise:thread-9")
	if err != nil || loaded.TipSHA != "tipsha" {
		t.Fatalf("supervise snapshot not durable: %+v err=%v", loaded, err)
	}

	// Complete flips it terminal so it is not resumed again.
	if err := cp.Complete(ctx, "supervise:thread-9", "build it", true); err != nil {
		t.Fatal(err)
	}
	sup2, _ := cp.InFlightSupervise(ctx)
	if len(sup2) != 0 {
		t.Errorf("a completed supervise run must not be in flight, got %+v", sup2)
	}
	if got, _ := s.GetTask(ctx, "supervise:thread-9"); got.Status != "done" {
		t.Errorf("after Complete, supervise status = %q, want done", got.Status)
	}
}

// --- small git helpers for the durability test (kept local; no shared test util) ---

func gitInit(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "init", "-q")
	git(t, repo, "checkout", "-q", "-b", "master")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeCommit(t *testing.T, repo, name, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "user.email=t@x", "-c", "user.name=t", "commit", "-q", "-m", msg)
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}
