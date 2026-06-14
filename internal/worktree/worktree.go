// Package worktree creates and tears down an isolated git worktree + branch per
// task, so every run is disposable by construction (CLAUDE.md §5; principle #5,
// "small, reversible, verified steps"). The orchestrator points the sandbox and
// verifier at the worktree path, then calls Cleanup when the task is done.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// current HEAD of baseRepo. On any failure it leaves nothing behind.
func Create(ctx context.Context, baseRepo, taskID string) (*Worktree, error) {
	if taskID == "" {
		return nil, fmt.Errorf("worktree: empty task id")
	}
	branch := "task/" + taskID

	tmpBase, err := os.MkdirTemp("", "nilcore-wt-")
	if err != nil {
		return nil, fmt.Errorf("worktree tempdir for %s: %w", taskID, err)
	}
	// git creates the leaf dir; it must not pre-exist.
	path := filepath.Join(tmpBase, strings.ReplaceAll(taskID, "/", "-"))

	if out, err := git(ctx, baseRepo, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		_ = os.RemoveAll(tmpBase) // no leaked worktree on partial create
		return nil, fmt.Errorf("create worktree for %s: %w (%s)", taskID, err, strings.TrimSpace(out))
	}
	return &Worktree{path: path, branch: branch, baseRepo: baseRepo, tmpBase: tmpBase}, nil
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

// git runs a git subcommand in dir and returns its combined output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
