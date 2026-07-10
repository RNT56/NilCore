// Package worktree creates and tears down an isolated git worktree + branch per
// task, so every run is disposable by construction (CLAUDE.md §5; principle #5,
// "small, reversible, verified steps"). The orchestrator points the sandbox and
// verifier at the worktree path, then calls Cleanup when the task is done.
//
// Host-side git here is hardening-clamped (invariant I4). A model can write into
// a worktree it owns — including .git/hooks and .git/config — so any git we run
// on the host (create off the tip, read HEAD, commit the result, integrate) must
// not let a repo-authored hook, fsmonitor binary, or external config execute.
// All invocations route through the single shared helper in internal/tools
// (HardenArgs + HardenedEnv) so the worktree and the `git` tool are neutralized
// identically.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"nilcore/internal/tools"
)

// Worktree is a checked-out git worktree on a fresh branch. Path is where the
// task runs; Cleanup removes both the worktree and the branch.
type Worktree struct {
	path     string
	branch   string
	baseRepo string
	tmpBase  string // the temp dir holding the worktree, removed on Cleanup
	baseSHA  string // the resolved start-point commit, for a since-create diff
}

// Path is the absolute worktree directory the task operates in.
func (w *Worktree) Path() string { return w.path }

// Branch is the task branch created for this worktree (e.g. "task/P1-T03").
func (w *Worktree) Branch() string { return w.branch }

// Create makes a worktree for taskID on a fresh "task/<taskID>" branch off the
// current HEAD of baseRepo. On any failure it leaves nothing behind. It is the
// frozen single-task entry point and delegates to CreateFrom with "HEAD", so the
// orchestrator's path is unchanged.
func Create(ctx context.Context, baseRepo, taskID string) (*Worktree, error) {
	if taskID == "" {
		return nil, fmt.Errorf("worktree: empty task id")
	}
	// The leaf must be a valid single dir name; slashes in the id (e.g. "task/x")
	// would otherwise create nested dirs under tmpBase.
	leaf := strings.ReplaceAll(taskID, "/", "-")
	return CreateFrom(ctx, baseRepo, "task/"+taskID, leaf, "HEAD")
}

// CreateFrom makes a worktree on a fresh `branch` off an arbitrary start-point
// committish (a SHA, ref, or "HEAD"). leaf is the directory name created under
// the temp base. This is the multi-agent seam: the DAG scheduler re-points each
// dependent's start-point at the current integration tip so dependents are coded
// on top of merged dependencies.
//
// It errors clearly — never panics — if the start-point does not resolve (an
// unknown SHA, or a fresh `git init` with no commits yet). Callers that need a
// greenfield repo must make an initial commit first (see project bootstrap).
func CreateFrom(ctx context.Context, baseRepo, branch, leaf, startPoint string) (*Worktree, error) {
	if branch == "" {
		return nil, fmt.Errorf("worktree: empty branch")
	}
	if leaf == "" {
		return nil, fmt.Errorf("worktree: empty leaf")
	}
	if startPoint == "" {
		return nil, fmt.Errorf("worktree: empty start-point for branch %s", branch)
	}

	// Resolve the start-point up front so an unresolvable committish (unknown SHA,
	// or an empty-HEAD greenfield repo) is a clear error, not a confusing
	// `worktree add` failure or a panic downstream. The resolved SHA is pinned on
	// the Worktree so DiffStat can report what changed since this exact baseline,
	// even after later commits advance the worktree's HEAD.
	baseSHA, err := git(ctx, baseRepo, "rev-parse", "--verify", "--quiet", startPoint+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("worktree: start-point %q does not resolve in %s (need an initial commit for a greenfield repo): %w", startPoint, baseRepo, err)
	}
	baseSHA = strings.TrimSpace(baseSHA)

	tmpBase, err := os.MkdirTemp("", "nilcore-wt-")
	if err != nil {
		return nil, fmt.Errorf("worktree tempdir for %s: %w", branch, err)
	}
	// git creates the leaf dir; it must not pre-exist.
	path := filepath.Join(tmpBase, leaf)

	if out, err := git(ctx, baseRepo, "worktree", "add", "-b", branch, path, startPoint); err != nil {
		_ = os.RemoveAll(tmpBase) // no leaked worktree on partial create
		return nil, fmt.Errorf("create worktree %s off %s: %w (%s)", branch, startPoint, err, strings.TrimSpace(out))
	}
	return &Worktree{path: path, branch: branch, baseRepo: baseRepo, tmpBase: tmpBase, baseSHA: baseSHA}, nil
}

// Head returns the commit SHA the worktree currently points at.
func (w *Worktree) Head(ctx context.Context) (string, error) {
	out, err := git(ctx, w.path, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("worktree head: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Commit stages all changes and commits them with message, returning the new
// HEAD SHA and whether anything was actually committed. On a clean tree it is a
// no-op and returns (currentHEAD, false, nil) — so the integrator can merge a
// branch that produced no diff without a spurious "nothing to commit" error.
//
// It uses the hardened env and pins an inert committer identity (the same one
// the `git` tool uses) so the commit never depends on host/global git config.
func (w *Worktree) Commit(ctx context.Context, message string) (sha string, changed bool, err error) {
	if message == "" {
		return "", false, fmt.Errorf("worktree commit: empty message")
	}
	if _, err := git(ctx, w.path, "add", "-A"); err != nil {
		return "", false, fmt.Errorf("worktree stage: %w", err)
	}

	// `diff --cached --quiet` exits 0 when the index matches HEAD (nothing to
	// commit) and 1 when it differs. A clean tree is a result, not an error.
	if _, derr := git(ctx, w.path, "diff", "--cached", "--quiet"); derr == nil {
		head, herr := w.Head(ctx)
		if herr != nil {
			return "", false, herr
		}
		return head, false, nil
	}

	if out, cerr := git(ctx, w.path,
		"-c", "user.email=agent@nilcore.local", "-c", "user.name=nilcore",
		"commit", "-m", message); cerr != nil {
		return "", false, fmt.Errorf("worktree commit: %w (%s)", cerr, strings.TrimSpace(out))
	}
	head, herr := w.Head(ctx)
	if herr != nil {
		return "", false, herr
	}
	return head, true, nil
}

// Merge folds ref into this worktree with `--no-ff`, committing the merge with an
// inert committer identity (the same one Commit uses, so it never depends on
// host/global git config). It distinguishes a genuine CONTENT CONFLICT (the branches
// do not combine — git leaves the tree mid-merge with unmerged paths; we run a clean
// `git merge --abort` and report conflict=true, err=nil) from a git FAULT (an unknown
// ref, a "not something we can merge" error, a corrupt repo — git fails WITHOUT
// entering the conflicted merge state; we report conflict=false and a non-nil err).
// Conflating the two hid real faults behind a bogus "conflict". It is the
// throwaway-re-base primitive (Phase 2): a multi-dep worker's base is built by
// CreateFrom(refs[0]) + Merge(each remaining ref). Committing per merge (rather than
// --no-commit) avoids a lingering MERGE_HEAD across sequential merges. It is NOT an
// integration — the verified merge stays the Integrator's job (I2). Hardened (I4):
// runs through the same clamped git helper as every other worktree op, so a merged-in
// branch's hooks/config can never execute on the host.
func (w *Worktree) Merge(ctx context.Context, ref, message string) (conflict bool, err error) {
	if w == nil {
		return false, fmt.Errorf("worktree merge: nil worktree")
	}
	if message == "" {
		message = "merge " + ref
	}
	_, merr := git(ctx, w.path,
		"-c", "user.email=agent@nilcore.local", "-c", "user.name=nilcore",
		"merge", "--no-ff", "-m", message, ref)
	if merr == nil {
		return false, nil
	}

	// git merge failed. Only a genuine content conflict leaves the tree mid-merge
	// (MERGE_HEAD present / unmerged index entries). A fault — unknown ref, refusal
	// to merge, corrupt repo — fails WITHOUT ever starting the merge, so there is
	// nothing to abort and it must surface as an error, not a phantom "conflict".
	if !w.mergeInProgress(ctx) {
		return false, fmt.Errorf("worktree merge %s failed (not a content conflict): %w", ref, merr)
	}

	// A real conflict left the tree mid-merge. Abort to restore the pre-merge tip
	// exactly; a failed abort is a real fault (the tree may be dirty) we surface so
	// the caller tears the throwaway down.
	if _, aerr := git(ctx, w.path, "merge", "--abort"); aerr != nil {
		return true, fmt.Errorf("worktree merge %s conflicted and abort failed: %w", ref, aerr)
	}
	return true, nil
}

// mergeInProgress reports whether the worktree is sitting in the conflicted state
// left by a failed `git merge` — i.e. a MERGE_HEAD exists. It is how Merge tells a
// genuine content conflict (merge started, then hit unmergeable hunks) apart from a
// git fault that never began a merge (unknown ref, refusal, corrupt repo). It reads
// through the hardened git helper (I4); a git error here is treated as "no merge in
// progress" so a fault degrades to the fault branch, never a false conflict.
func (w *Worktree) mergeInProgress(ctx context.Context) bool {
	out, err := git(ctx, w.path, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// Checkout re-points this worktree at ref in DETACHED mode, forcing the working
// tree to match (discarding any tracked drift). It is how a long-lived READ worktree
// tracks the moving integration tip: --detach avoids the "branch already checked out
// in another worktree" conflict, and --force makes it a clean re-sync to the new tip.
// Hardened (I4): the merged-in tip's repo-authored hooks/config can never execute on
// the host. The worktree must hold no edits worth keeping (a read tree never does).
func (w *Worktree) Checkout(ctx context.Context, ref string) error {
	if w == nil {
		return fmt.Errorf("worktree checkout: nil worktree")
	}
	if ref == "" {
		return fmt.Errorf("worktree checkout: empty ref")
	}
	if out, err := git(ctx, w.path, "checkout", "--detach", "--force", ref); err != nil {
		return fmt.Errorf("worktree checkout %s: %w (%s)", ref, err, strings.TrimSpace(out))
	}
	return nil
}

// Reset hard-resets this worktree's branch to sha (discarding the working tree + index).
// It is how an integrator UNDOES a merge whose integrated tip failed verification —
// restoring the last verified tip so the next child merges onto a green tree (I2). Unlike
// Checkout it stays ON the branch (no detach), so subsequent merges advance the branch.
// Hardened git (I4): no repo-authored hook/config runs on the host.
func (w *Worktree) Reset(ctx context.Context, sha string) error {
	if w == nil {
		return fmt.Errorf("worktree reset: nil worktree")
	}
	if sha == "" {
		return fmt.Errorf("worktree reset: empty sha")
	}
	if out, err := git(ctx, w.path, "reset", "--hard", sha); err != nil {
		return fmt.Errorf("worktree reset %s: %w (%s)", sha, err, strings.TrimSpace(out))
	}
	return nil
}

// ListFiles returns the worktree's tracked file paths (git ls-files) — a cheap,
// bounded view of the tree's STRUCTURE (not contents) for grounding the supervisor's
// answer in "where things are". The result is byte-capped to maxBytes (on a line
// boundary) so a huge repo can never bloat a prompt; a non-positive maxBytes applies
// a default. Hardened git (I4). Empty (not an error) when the tree has no tracked
// files yet.
func (w *Worktree) ListFiles(ctx context.Context, maxBytes int) (string, error) {
	if w == nil {
		return "", nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultListFilesBytes
	}
	out, err := git(ctx, w.path, "ls-files")
	if err != nil {
		return "", fmt.Errorf("worktree ls-files: %w", err)
	}
	return clampBytes(strings.TrimSpace(out), maxBytes), nil
}

// defaultListFilesBytes bounds a ListFiles report when the caller passes a
// non-positive cap — enough for a real file tree, small enough not to bloat a prompt.
const defaultListFilesBytes = 3072

// WorkingDiff reports a worker's UNCOMMITTED work-in-progress: the diff of the
// working tree against the worktree's create-time baseline (tracked edits, committed
// or not) plus the names of any new untracked files. It is the consistent "here is
// what I've done so far" snapshot a subagent attaches when it asks the supervisor —
// consistent because a worker only asks while PARKED on the blocking call, so its
// tree is quiescent at read time (no torn read; no scratch ref needed). Read-only
// (never stages or commits), bounded to maxBytes (on a line boundary), hardened git
// (I4). Empty (not an error) when the worker has changed nothing yet.
func (w *Worktree) WorkingDiff(ctx context.Context, maxBytes int) (string, error) {
	if w == nil {
		return "", nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultWorkingDiffBytes
	}
	base := w.baseSHA
	if base == "" {
		base = "HEAD"
	}
	// Tracked modifications since the baseline — `git diff <commit>` compares the
	// WORKING TREE to that commit, so uncommitted edits are included.
	diff, err := git(ctx, w.path, "diff", base)
	if err != nil {
		return "", fmt.Errorf("worktree working diff: %w", err)
	}
	// New untracked files (names only — read-only; we never `git add`).
	untracked, _ := git(ctx, w.path, "ls-files", "--others", "--exclude-standard")

	var b strings.Builder
	if d := strings.TrimSpace(diff); d != "" {
		b.WriteString(d)
	}
	if u := strings.TrimSpace(untracked); u != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("new (untracked) files:\n")
		b.WriteString(u)
	}
	if b.Len() == 0 {
		return "", nil
	}
	return clampBytes(b.String(), maxBytes), nil
}

// defaultWorkingDiffBytes bounds a WorkingDiff snapshot so a large work-in-progress
// can never bloat the supervisor's answer prompt; the worker's own question carries
// the specific concern, this is the supporting context.
const defaultWorkingDiffBytes = 6144

// DiffStat reports WHAT CHANGED in this worktree since it was created: the
// changed-file name-status list plus the `git diff --stat` summary, taken
// between the pinned create-time start-point (baseSHA) and the worktree's
// current committed state. It is a bounded, host-side report the orchestrator
// can hand a supervisor as a concise "here is what the subagent did" — never a
// transcript. The whole report is truncated to at most maxBytes bytes (on a
// line boundary where possible) so a large refactor cannot bloat the prompt; a
// non-positive maxBytes applies defaultDiffStatBytes. An empty diff (the worker
// changed nothing) returns "" with a nil error.
//
// It diffs the pinned commit against HEAD (the worker's verified, committed
// state), so it must be called after Commit; it never reads the working tree, so
// it is deterministic regardless of any uncommitted scratch the worker left.
func (w *Worktree) DiffStat(ctx context.Context, maxBytes int) (string, error) {
	if w == nil {
		return "", nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultDiffStatBytes
	}
	base := w.baseSHA
	if base == "" {
		base = "HEAD" // no pinned baseline: degrade to "nothing since HEAD" (empty)
	}

	// name-status: a compact per-file changed-file list (A/M/D + path).
	names, err := git(ctx, w.path, "diff", "--name-status", base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("worktree diff name-status: %w", err)
	}
	// --stat: the per-file insertion/deletion summary + the totals line.
	stat, err := git(ctx, w.path, "diff", "--stat", base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("worktree diff stat: %w", err)
	}

	names, stat = strings.TrimSpace(names), strings.TrimSpace(stat)
	if names == "" && stat == "" {
		return "", nil // the worker changed nothing
	}

	var b strings.Builder
	if names != "" {
		b.WriteString("Changed files:\n")
		b.WriteString(names)
	}
	if stat != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(stat)
	}
	return clampBytes(b.String(), maxBytes), nil
}

// defaultDiffStatBytes bounds a DiffStat report when the caller passes a
// non-positive cap. It is generous enough for a real changed-file list and stat
// summary, but small enough that a sprawling refactor cannot bloat a prompt.
const defaultDiffStatBytes = 4096

// clampBytes truncates s to at most n bytes, preferring to cut on the last
// newline within the budget so the report never ends mid-line, and appends a
// short, bounded elision marker so a reader knows the report was clipped. It
// operates on bytes (not runes) but never splits a multi-byte rune at the cut
// because it backs up to a newline; if no newline fits it cuts at a rune
// boundary so the result stays valid UTF-8.
func clampBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], '\n')
	if cut <= 0 {
		// No newline in budget: back up to a rune boundary so we never emit a
		// half-encoded rune.
		cut = n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	return s[:cut] + "\n… (diff truncated)"
}

// Cleanup removes the worktree and deletes its branch. It is idempotent (safe to
// call more than once, and after a partial create) and uses a background context
// so it still runs when the task's own context was cancelled.
func (w *Worktree) Cleanup() error {
	if w == nil {
		return nil
	}
	ctx := context.Background()

	if _, err := os.Stat(w.path); err == nil {
		if _, err := git(ctx, w.baseRepo, "worktree", "remove", "--force", w.path); err != nil {
			_ = os.RemoveAll(w.path) // fall back to a plain delete
		}
	}
	// Drop the now-stale admin entry and the task branch (best effort: the
	// branch may already be gone, or never created).
	_, _ = git(ctx, w.baseRepo, "worktree", "prune")
	_, _ = git(ctx, w.baseRepo, "branch", "-D", w.branch)
	_ = os.RemoveAll(w.tmpBase)

	if _, err := os.Stat(w.path); err == nil {
		return fmt.Errorf("worktree %s still present after cleanup", w.path)
	}
	return nil
}

// Release removes the worktree's checkout (directory + admin entry + scratch) but
// KEEPS its branch, so a dependent worker can still cut a new worktree from it
// (the DependsOn-propagation seam). The branch is reclaimed later by the wave's
// end-of-run sweep (DeleteBranches) — branches are cheap refs. Idempotent.
func (w *Worktree) Release() error {
	if w == nil {
		return nil
	}
	ctx := context.Background()
	if _, err := os.Stat(w.path); err == nil {
		if _, err := git(ctx, w.baseRepo, "worktree", "remove", "--force", w.path); err != nil {
			_ = os.RemoveAll(w.path)
		}
	}
	_, _ = git(ctx, w.baseRepo, "worktree", "prune")
	_ = os.RemoveAll(w.tmpBase)
	if _, err := os.Stat(w.path); err == nil {
		return fmt.Errorf("worktree %s still present after release", w.path)
	}
	return nil
}

// DeleteBranches removes every local branch under prefix (e.g. "task/") in
// baseRepo — the end-of-run sweep for worker branches kept alive by Release so
// dependents could branch from them. Best-effort: a branch already gone is fine.
func DeleteBranches(ctx context.Context, baseRepo, prefix string) {
	out, err := git(ctx, baseRepo, "branch", "--list", prefix+"*", "--format=%(refname:short)")
	if err != nil {
		return
	}
	for _, b := range strings.Split(strings.TrimSpace(out), "\n") {
		if b = strings.TrimSpace(b); b != "" {
			_, _ = git(ctx, baseRepo, "branch", "-D", b)
		}
	}
}

// ListBranches returns the short-names of every local branch under prefix (e.g.
// "suspend/") in baseRepo. It is the read half of the durable-anchor sweep
// (agent.Checkpoint.SweepSuspended): the suspend/ recovery anchors a nap leaves behind
// accumulate across restarts and must be reclaimed once resolved. It mirrors
// DeleteBranches' `branch --list <prefix>*` query and runs the same hardened git (I4),
// so a repo-authored hook/config can never execute on the host. Empty (not an error)
// when nothing matches the prefix.
func ListBranches(ctx context.Context, baseRepo, prefix string) ([]string, error) {
	out, err := git(ctx, baseRepo, "branch", "--list", prefix+"*", "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("list branches %s*: %w", prefix, err)
	}
	var branches []string
	for _, b := range strings.Split(strings.TrimSpace(out), "\n") {
		if b = strings.TrimSpace(b); b != "" {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

// PinBranch force-creates (or moves) `branch` to point at `sha` in baseRepo. It is
// the durable-resume anchor: the run-end branch sweep (DeleteBranches) only reclaims
// the throwaway task/ rebase/ integrate/ read/ prefixes, so a branch under a
// different prefix (e.g. resume/<taskID>) keeps the verified integration tip
// reachable across a graceful restart — even after its integrate/ branch is swept
// and its worktree torn down. `git branch -f` is idempotent (creates the ref or
// moves it), so a re-pin on every integration checkpoint just advances the ref.
// Hardened git (I4): a repo-authored hook/config can never execute on the host.
func PinBranch(ctx context.Context, baseRepo, branch, sha string) error {
	if branch == "" || sha == "" {
		return fmt.Errorf("worktree: pin branch requires a branch and a sha")
	}
	if out, err := git(ctx, baseRepo, "branch", "-f", branch, sha); err != nil {
		return fmt.Errorf("pin branch %s @ %s: %w (%s)", branch, sha, err, strings.TrimSpace(out))
	}
	return nil
}

// DeleteBranch removes a single branch by exact name in baseRepo (best-effort: a
// branch already gone is fine). It is the teardown for a durable resume pin once the
// run it anchored has converged — the one-ref counterpart to DeleteBranches' prefix
// sweep, which never touches the resume/ prefix.
func DeleteBranch(ctx context.Context, baseRepo, branch string) {
	if branch == "" {
		return
	}
	_, _ = git(ctx, baseRepo, "branch", "-D", branch)
}

// Prunable returns the paths of worktrees registered to baseRepo whose checkout
// directory no longer exists — left behind by a crashed prior process. These are
// the ONLY worktrees safe to reclaim blindly: a live worktree's directory is
// present, so it is never listed (`git worktree list --porcelain` marks a gone
// worktree with a `prunable` line). Safe to call with other NilCore processes
// running — their live worktrees are not prunable.
func Prunable(ctx context.Context, baseRepo string) ([]string, error) {
	out, err := git(ctx, baseRepo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var prunable []string
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "prunable") && cur != "":
			prunable = append(prunable, cur)
		case line == "":
			cur = ""
		}
	}
	return prunable, nil
}

// Prune drops the administrative entries of worktrees whose directories are
// already gone (`git worktree prune`). It never removes a live worktree, so it is
// safe at startup even with other NilCore processes active.
func Prune(ctx context.Context, baseRepo string) error {
	_, err := git(ctx, baseRepo, "worktree", "prune")
	return err
}

// Diff returns the unified diff of branch against the base repo's current HEAD —
// the change a converged integration branch would promote. It runs the hardened
// git (I4) so a repo-authored hook/config can never execute on the host.
func Diff(ctx context.Context, baseRepo, branch string) (string, error) {
	return git(ctx, baseRepo, "diff", "HEAD.."+branch)
}

// DiffPreview reports what landing branch onto the base repo's current HEAD would
// change, as a BOUNDED operator preview: the `git diff --stat` summary followed by
// the head of the unified diff, the whole report clamped to maxBytes (on a line
// boundary — clampBytes) so a sprawling branch can never flood a terminal. It uses
// the same HEAD..branch endpoints as Diff (the change a promote would land). The
// branch is resolved first so a swept/renamed ref is a clear error rather than a
// confusing empty diff; an identical tree returns "" with a nil error. Read-only,
// hardened git (I4). A non-positive maxBytes applies defaultDiffPreviewBytes.
func DiffPreview(ctx context.Context, baseRepo, branch string, maxBytes int) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("worktree diff preview: empty branch")
	}
	if maxBytes <= 0 {
		maxBytes = defaultDiffPreviewBytes
	}
	if _, err := git(ctx, baseRepo, "rev-parse", "--verify", "--quiet", branch+"^{commit}"); err != nil {
		return "", fmt.Errorf("branch %q does not resolve in %s: %w", branch, baseRepo, err)
	}
	stat, err := git(ctx, baseRepo, "diff", "--stat", "HEAD.."+branch)
	if err != nil {
		return "", fmt.Errorf("worktree diff preview stat: %w", err)
	}
	full, err := Diff(ctx, baseRepo, branch)
	if err != nil {
		return "", fmt.Errorf("worktree diff preview: %w", err)
	}
	stat, full = strings.TrimSpace(stat), strings.TrimSpace(full)
	if stat == "" && full == "" {
		return "", nil // the branch lands nothing
	}
	var b strings.Builder
	if stat != "" {
		b.WriteString(stat)
	}
	if full != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(full)
	}
	return clampBytes(b.String(), maxBytes), nil
}

// defaultDiffPreviewBytes bounds a DiffPreview when the caller passes no cap —
// generous enough to read a real change, small enough for one terminal screenful
// scroll.
const defaultDiffPreviewBytes = 16 << 10

// git runs a hardening-clamped git subcommand in dir and returns its combined
// output. The clamp (HardenArgs `-c` flags + HardenedEnv) is identical to the
// `git` tool's, so a repo-authored hook/fsmonitor/config can never execute on the
// host (I4). Both halves of the clamp must always travel together.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	full := append(tools.HardenArgs(), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = tools.HardenedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
