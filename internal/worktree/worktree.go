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
// host/global git config). It distinguishes a CONFLICT (the branches do not
// combine — git exits non-zero; we run a clean `git merge --abort` and report
// conflict=true) from a git FAULT (err set). It is the throwaway-re-base primitive
// (Phase 2): a multi-dep worker's base is built by CreateFrom(refs[0]) + Merge(each
// remaining ref). Committing per merge (rather than --no-commit) avoids a lingering
// MERGE_HEAD across sequential merges. It is NOT an integration — the verified
// merge stays the Integrator's job (I2). Hardened (I4): runs through the same
// clamped git helper as every other worktree op, so a merged-in branch's
// hooks/config can never execute on the host.
func (w *Worktree) Merge(ctx context.Context, ref, message string) (conflict bool, err error) {
	if w == nil {
		return false, fmt.Errorf("worktree merge: nil worktree")
	}
	if message == "" {
		message = "merge " + ref
	}
	if _, merr := git(ctx, w.path,
		"-c", "user.email=agent@nilcore.local", "-c", "user.name=nilcore",
		"merge", "--no-ff", "-m", message, ref); merr != nil {
		// A merge that does not apply cleanly leaves the tree mid-merge. Abort to
		// restore the pre-merge tip exactly; a failed abort is a real fault (the tree
		// may be dirty) we surface so the caller tears the throwaway down.
		if _, aerr := git(ctx, w.path, "merge", "--abort"); aerr != nil {
			return true, fmt.Errorf("worktree merge %s conflicted and abort failed: %w", ref, aerr)
		}
		return true, nil
	}
	return false, nil
}

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
