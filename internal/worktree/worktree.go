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

	"nilcore/internal/tools"
)

// Worktree is a checked-out git worktree on a fresh branch. Path is where the
// task runs; Cleanup removes both the worktree and the branch.
type Worktree struct {
	path     string
	branch   string
	baseRepo string
	tmpBase  string // the temp dir holding the worktree, removed on Cleanup
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
	// `worktree add` failure or a panic downstream.
	if _, err := git(ctx, baseRepo, "rev-parse", "--verify", "--quiet", startPoint+"^{commit}"); err != nil {
		return nil, fmt.Errorf("worktree: start-point %q does not resolve in %s (need an initial commit for a greenfield repo): %w", startPoint, baseRepo, err)
	}

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
	return &Worktree{path: path, branch: branch, baseRepo: baseRepo, tmpBase: tmpBase}, nil
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
