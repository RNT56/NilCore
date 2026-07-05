package main

// chat_delivery_test.go covers the delivery loop (/diff, /apply, kept-branch
// upkeep) hermetically: a temp git repo stands in for the operator's checkout, a
// real session.Session carries WorkState.Branch, and scripted approvers exercise
// the PromoteToBase gate's approve/deny arms without a human or a model.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/policy"
	"nilcore/internal/session"
	"nilcore/internal/termui"
	"nilcore/internal/verb"
)

// allowApprover approves every gate — the /apply approved arm. (denyApprover, the
// deny arm, already lives in build_test.go.)
type allowApprover struct{}

func (allowApprover) Approve(string) bool { return true }

// deliveryGit runs RAW git in dir for test fixture setup (the production paths
// under test run the hardened chatGit; fixtures need env control, e.g. committer
// dates for the cap's newest-first ordering).
func deliveryGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newDeliveryRepo makes a repo on branch main with one committed file.
func newDeliveryRepo(t *testing.T) string {
	t.Helper()
	requireGitOnPath(t)
	repo := t.TempDir()
	deliveryGit(t, repo, nil, "init", "-q", "-b", "main")
	writeDeliveryFile(t, repo, "a.txt", "base\n")
	deliveryGit(t, repo, nil, "add", "-A")
	deliveryGit(t, repo, nil, "-c", "user.email=t@nilcore.local", "-c", "user.name=t",
		"commit", "-q", "-m", "base")
	return repo
}

func requireGitOnPath(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func writeDeliveryFile(t *testing.T, repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// addKeptBranch cuts branch off HEAD with one commit changing a.txt to body, then
// returns main to its prior tip. Returns the branch tip SHA.
func addKeptBranch(t *testing.T, repo, branch, body string) string {
	t.Helper()
	deliveryGit(t, repo, nil, "checkout", "-q", "-b", branch)
	writeDeliveryFile(t, repo, "a.txt", body)
	deliveryGit(t, repo, nil, "add", "-A")
	deliveryGit(t, repo, nil, "-c", "user.email=t@nilcore.local", "-c", "user.name=t",
		"commit", "-q", "-m", "kept edit")
	sha := deliveryGit(t, repo, nil, "rev-parse", "HEAD")
	deliveryGit(t, repo, nil, "checkout", "-q", "main")
	return sha
}

// deliverySession builds a real Idle session carrying branch in its WorkState.
func deliverySession(repo, branch string) *session.Session {
	s := session.New(chatConvoID, chatPrincipal, repo, nil)
	s.State.Branch = branch
	return s
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", branch+"^{commit}")
	cmd.Dir = repo
	return cmd.Run() == nil
}

// --- pinKeptBranch: sweep-proof re-homing ------------------------------------

func TestPinKeptBranch(t *testing.T) {
	repo := newDeliveryRepo(t)
	tip := addKeptBranch(t, repo, "task/chat-local-7", "edited\n")
	ctx := context.Background()

	kept := pinKeptBranch(ctx, repo, "task/chat-local-7", nil)
	if kept != "nilcore/kept/chat-local-7" {
		t.Fatalf("pinKeptBranch = %q, want nilcore/kept/chat-local-7", kept)
	}
	if branchExists(t, repo, "task/chat-local-7") {
		t.Error("the throwaway task/ ref must be dropped after the pin")
	}
	if got := deliveryGit(t, repo, nil, "rev-parse", kept); got != tip {
		t.Errorf("kept branch tip = %s, want %s (same commit)", got, tip)
	}

	// Idempotent: an already-kept name passes through untouched.
	if again := pinKeptBranch(ctx, repo, kept, nil); again != kept {
		t.Errorf("re-pin of a kept branch = %q, want %q", again, kept)
	}
	// Best-effort: an unresolvable branch keeps its original name (never fails).
	if got := pinKeptBranch(ctx, repo, "task/gone", nil); got != "task/gone" {
		t.Errorf("pin of a missing branch = %q, want the original name back", got)
	}
	if pinKeptBranch(ctx, repo, "", nil) != "" {
		t.Error("pin of an empty branch must stay empty")
	}
}

// --- capKeptBranches: bounded family, newest kept, active immune --------------

func TestCapKeptBranchesPrunesOldest(t *testing.T) {
	repo := newDeliveryRepo(t)
	ctx := context.Background()

	// keptBranchCap+3 kept branches at commits with strictly increasing committer
	// dates, b01 oldest … bNN newest, so for-each-ref's newest-first order is exact.
	n := keptBranchCap + 3
	names := make([]string, n)
	for i := 0; i < n; i++ {
		writeDeliveryFile(t, repo, "a.txt", strings.Repeat("x", i+1)+"\n")
		date := "2026-01-01T00:00:" + string(rune('0'+(i/10)%6)) + string(rune('0'+i%10)) + "+0000"
		env := []string{"GIT_COMMITTER_DATE=" + date, "GIT_AUTHOR_DATE=" + date}
		deliveryGit(t, repo, env, "add", "-A")
		deliveryGit(t, repo, env, "-c", "user.email=t@nilcore.local", "-c", "user.name=t",
			"commit", "-q", "-m", "c")
		names[i] = "nilcore/kept/b" + string(rune('0'+(i+1)/10)) + string(rune('0'+(i+1)%10))
		deliveryGit(t, repo, nil, "branch", names[i])
	}

	// The 4th-oldest is the live conversation's branch: old enough to be pruned by
	// age, but Active must shield it.
	active := names[3]
	capKeptBranches(ctx, repo, active, nil)

	// Oldest 3 pruned; everything newer (and the active one) survives.
	for i, name := range names {
		got := branchExists(t, repo, name)
		want := i >= 3 // 0,1,2 pruned (oldest); 3 is active (kept); rest within cap
		if got != want {
			t.Errorf("branch %s exists=%v, want %v", name, got, want)
		}
	}
}

// --- /diff -------------------------------------------------------------------

func TestApplyDiffVerbNoBranch(t *testing.T) {
	repo := newDeliveryRepo(t)
	var out strings.Builder
	applyDiffVerb(context.Background(), deliverySession(repo, ""), termui.New(&out))
	if !strings.Contains(out.String(), "nothing to preview") {
		t.Fatalf("no-branch /diff must say so; got:\n%s", out.String())
	}
}

func TestApplyDiffVerbPreview(t *testing.T) {
	repo := newDeliveryRepo(t)
	addKeptBranch(t, repo, "nilcore/kept/chat-local-1", "changed\n")
	var out strings.Builder
	applyDiffVerb(context.Background(), deliverySession(repo, "nilcore/kept/chat-local-1"), termui.New(&out))
	s := out.String()
	if !strings.Contains(s, "kept branch: nilcore/kept/chat-local-1") ||
		!strings.Contains(s, "a.txt") || !strings.Contains(s, "+changed") {
		t.Fatalf("/diff preview missing branch/stat/diff; got:\n%s", s)
	}
}

func TestApplyDiffVerbGoneBranch(t *testing.T) {
	repo := newDeliveryRepo(t)
	var out strings.Builder
	applyDiffVerb(context.Background(), deliverySession(repo, "nilcore/kept/gone"), termui.New(&out))
	if !strings.Contains(out.String(), "cannot preview") {
		t.Fatalf("a pruned branch must report cleanly; got:\n%s", out.String())
	}
}

// --- /apply: approve ⇒ merged tip + cleared state ------------------------------

func TestApplyApplyVerbApproved(t *testing.T) {
	repo := newDeliveryRepo(t)
	tip := addKeptBranch(t, repo, "nilcore/kept/chat-local-1", "changed\n")
	sess := deliverySession(repo, "nilcore/kept/chat-local-1")
	var out strings.Builder

	applyApplyVerb(context.Background(), sess, termui.New(&out), allowApprover{}, nil)

	if got := deliveryGit(t, repo, nil, "rev-parse", "HEAD"); got != tip {
		t.Errorf("main tip = %s, want the kept tip %s (fast-forward)", got, tip)
	}
	if sess.KeptBranch() != "" {
		t.Error("WorkState.Branch must clear once the work landed")
	}
	if branchExists(t, repo, "nilcore/kept/chat-local-1") {
		t.Error("the landed kept ref must be reclaimed")
	}
	if s := out.String(); !strings.Contains(s, "applied nilcore/kept/chat-local-1") || !strings.Contains(s, "main") {
		t.Errorf("/apply must report the landed base + tip; got:\n%s", s)
	}
}

// --- /apply: deny ⇒ nothing moves, branch kept ---------------------------------

func TestApplyApplyVerbDenied(t *testing.T) {
	repo := newDeliveryRepo(t)
	base := deliveryGit(t, repo, nil, "rev-parse", "HEAD")
	addKeptBranch(t, repo, "nilcore/kept/chat-local-1", "changed\n")
	sess := deliverySession(repo, "nilcore/kept/chat-local-1")
	var out strings.Builder

	applyApplyVerb(context.Background(), sess, termui.New(&out), denyApprover{}, nil)

	if got := deliveryGit(t, repo, nil, "rev-parse", "HEAD"); got != base {
		t.Errorf("a denied apply moved HEAD: %s → %s", base, got)
	}
	if sess.KeptBranch() != "nilcore/kept/chat-local-1" {
		t.Error("a denied apply must keep WorkState.Branch")
	}
	if !branchExists(t, repo, "nilcore/kept/chat-local-1") {
		t.Error("a denied apply must keep the branch ref")
	}
	if !strings.Contains(out.String(), "apply denied") {
		t.Errorf("deny must be reported; got:\n%s", out.String())
	}
}

// A nil approver (no gate wired) fails CLOSED — no ambient authority (I3).
func TestApplyApplyVerbNilGateDenies(t *testing.T) {
	repo := newDeliveryRepo(t)
	base := deliveryGit(t, repo, nil, "rev-parse", "HEAD")
	addKeptBranch(t, repo, "nilcore/kept/chat-local-1", "changed\n")
	var out strings.Builder

	applyApplyVerb(context.Background(), deliverySession(repo, "nilcore/kept/chat-local-1"),
		termui.New(&out), nil, nil)

	if got := deliveryGit(t, repo, nil, "rev-parse", "HEAD"); got != base {
		t.Errorf("a nil-gate apply moved HEAD: %s → %s", base, got)
	}
	if !strings.Contains(out.String(), "apply denied") {
		t.Errorf("nil gate must deny; got:\n%s", out.String())
	}
}

// --- /apply: conflict ⇒ clean report, branch kept, no partial state -------------

func TestApplyApplyVerbConflict(t *testing.T) {
	repo := newDeliveryRepo(t)
	addKeptBranch(t, repo, "nilcore/kept/chat-local-1", "kept side\n")
	// Diverge main with a CONFLICTING edit to the same file.
	writeDeliveryFile(t, repo, "a.txt", "main side\n")
	deliveryGit(t, repo, nil, "add", "-A")
	deliveryGit(t, repo, nil, "-c", "user.email=t@nilcore.local", "-c", "user.name=t",
		"commit", "-q", "-m", "diverge")
	base := deliveryGit(t, repo, nil, "rev-parse", "HEAD")
	sess := deliverySession(repo, "nilcore/kept/chat-local-1")
	var out strings.Builder

	applyApplyVerb(context.Background(), sess, termui.New(&out), allowApprover{}, nil)

	if !strings.Contains(out.String(), "merge conflict") {
		t.Fatalf("conflict must be reported cleanly; got:\n%s", out.String())
	}
	if got := deliveryGit(t, repo, nil, "rev-parse", "HEAD"); got != base {
		t.Errorf("a conflicted apply moved HEAD: %s → %s", base, got)
	}
	// No partial state: the aborted merge leaves no MERGE_HEAD behind.
	cmd := exec.Command("git", "rev-parse", "-q", "--verify", "MERGE_HEAD")
	cmd.Dir = repo
	if cmd.Run() == nil {
		t.Error("MERGE_HEAD still present — the conflicted merge was not aborted")
	}
	if sess.KeptBranch() != "nilcore/kept/chat-local-1" || !branchExists(t, repo, "nilcore/kept/chat-local-1") {
		t.Error("a conflicted apply must keep the branch and the carried state")
	}
}

// --- /apply: no branch / mid-drive refusals -----------------------------------

func TestApplyApplyVerbNoBranch(t *testing.T) {
	repo := newDeliveryRepo(t)
	var out strings.Builder
	applyApplyVerb(context.Background(), deliverySession(repo, ""), termui.New(&out), allowApprover{}, nil)
	if !strings.Contains(out.String(), "nothing to apply") {
		t.Fatalf("no-branch /apply must say so; got:\n%s", out.String())
	}
}

// --- the REPL wiring: /apply's gate answer arrives over the lines channel -------

// TestChatREPLApplyGateViaLines drives /apply end-to-end through chatREPL: the
// verb parses via the SHARED parser, the PromoteToBase gate blocks on the REPL's
// OWN lines channel (replApprover — no second stdin reader), a typed "y" approves,
// and the kept branch lands on main. This is the delivery loop as the operator
// experiences it.
func TestChatREPLApplyGateViaLines(t *testing.T) {
	repo := newDeliveryRepo(t)
	tip := addKeptBranch(t, repo, "nilcore/kept/chat-local-1", "changed\n")
	sess := deliverySession(repo, "nilcore/kept/chat-local-1")

	r := newScriptReader("/apply", "y", "/quit")
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- chatREPL(context.Background(), sess, r, termui.New(&out), nil, nil) }()

	r.next() // /apply — parses, resolves, parks on the gate
	r.next() // y — the gate answer, consumed from the SAME lines channel
	r.next() // /quit

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("chatREPL returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chatREPL did not return — the /apply gate wedged the loop")
	}

	if got := deliveryGit(t, repo, nil, "rev-parse", "HEAD"); got != tip {
		t.Errorf("main tip = %s, want the kept tip %s", got, tip)
	}
	if sess.KeptBranch() != "" {
		t.Error("WorkState.Branch must clear after the approved apply")
	}
	s := out.String()
	// The gate must name the merge TARGET (main), not the kept source — that is what
	// the I2 "never auto-approve main/prod" floor keys off (GradedApprover.scopeFor
	// reads GateAction.Branch). The source rides in the detail; the result names it.
	if !strings.Contains(s, "gate — promote-to-base main") ||
		!strings.Contains(s, "from nilcore/kept/chat-local-1 to main") ||
		!strings.Contains(s, "applied nilcore/kept/chat-local-1") {
		t.Errorf("REPL did not surface the gate + result; got:\n%s", s)
	}
}

// captureApprover records the exact GateAction it is asked to approve (via the
// StructuredApprover opt-in GateStructured uses) so a test can assert what the
// "never auto-approve main/prod" floor actually sees. It approves, so the apply
// proceeds and the recorded action is the one that flowed through the gate.
type captureApprover struct{ got policy.GateAction }

func (c *captureApprover) Approve(string) bool { return true }
func (c *captureApprover) ApproveStructured(a policy.GateAction) bool {
	c.got = a
	return true
}

// TestApplyGateActionBranchIsTarget locks FIX #14: on a repo checked out on main,
// /apply must build a GateAction whose Branch is the merge TARGET (main) — the value
// GradedApprover.scopeFor keys the structural main/prod floor off — NOT the harmless
// kept source. The source rides in Detail for the audit trail. Were Branch the kept
// name, an operator envelope allowing nilcore/kept/* could auto-merge into main.
func TestApplyGateActionBranchIsTarget(t *testing.T) {
	repo := newDeliveryRepo(t)
	addKeptBranch(t, repo, "nilcore/kept/chat-local-9", "changed\n")
	sess := deliverySession(repo, "nilcore/kept/chat-local-9")

	cap := &captureApprover{}
	var out strings.Builder
	applyApplyVerb(context.Background(), sess, termui.New(&out), cap, nil)

	if cap.got.Type != policy.PromoteToBase {
		t.Fatalf("gate action type = %v, want PromoteToBase", cap.got.Type)
	}
	if cap.got.Branch != "main" {
		t.Errorf("gate Branch = %q, want the merge TARGET %q (so isProtectedBase fires)", cap.got.Branch, "main")
	}
	if !strings.Contains(cap.got.Detail, "nilcore/kept/chat-local-9") {
		t.Errorf("gate Detail must carry the source branch; got %q", cap.got.Detail)
	}
}

// --- emitDriveResult: the delivery hint line -----------------------------------

func TestEmitDriveResultKeptBranchLine(t *testing.T) {
	var out strings.Builder
	con := termui.New(&out)
	em := termui.NewEmitter(con, verb.General)

	emitDriveResult(em, true, "fixed the typo", "nilcore/kept/chat-local-2")
	s := out.String()
	if !strings.Contains(s, "verified — fixed the typo") ||
		!strings.Contains(s, "verified work kept on branch nilcore/kept/chat-local-2") ||
		!strings.Contains(s, "/diff to preview, /apply to merge") {
		t.Fatalf("kept-branch hint line missing; got:\n%s", s)
	}

	// No branch (read-only / unverified) ⇒ no hint line.
	out.Reset()
	emitDriveResult(em, true, "answered", "")
	if strings.Contains(out.String(), "kept on branch") {
		t.Fatalf("hint line must not render without a branch; got:\n%s", out.String())
	}
	out.Reset()
	emitDriveResult(em, false, "failed", "task/x")
	if strings.Contains(out.String(), "kept on branch") {
		t.Fatalf("hint line must not render unverified; got:\n%s", out.String())
	}
}
