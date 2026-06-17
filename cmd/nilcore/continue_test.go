package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/worktree"
)

// TestPreserveFailedAttemptCommitsWIP proves the enabling prerequisite for
// continue_from: a failed/incomplete worker's work-in-progress is committed to its
// task/<id> branch (the worker otherwise commits only on green, discarding its WIP
// with the released worktree), so a retry cut from that branch SEES the partial work.
// A clean / no-change tree preserves nothing (degrading to the prior discard behavior).
func TestPreserveFailedAttemptCommitsWIP(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repo := newGoRepo(t)

	wt, err := worktree.CreateFrom(ctx, repo, "task/x", "task-x", "HEAD")
	if err != nil {
		t.Fatalf("CreateFrom: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// A fresh worktree with no worker edits: nothing to preserve.
	if br := preserveFailedAttempt(ctx, wt); br != "" {
		t.Errorf("clean tree should preserve nothing, got branch %q", br)
	}

	// Simulate a worker's partial (failing) WIP, then preserve it.
	if err := os.WriteFile(filepath.Join(wt.Path(), "partial.go"), []byte("package x // incomplete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	br := preserveFailedAttempt(ctx, wt)
	if br != "task/x" {
		t.Fatalf("preserveFailedAttempt = %q, want task/x (the branch carrying the WIP)", br)
	}

	// A continue_from retry is cut from that branch — it must SEE the preserved WIP.
	cont, err := worktree.CreateFrom(ctx, repo, "task/xb", "task-xb", "task/x")
	if err != nil {
		t.Fatalf("continue-from worktree off the preserved branch: %v", err)
	}
	defer func() { _ = cont.Cleanup() }()
	if b, _ := os.ReadFile(filepath.Join(cont.Path(), "partial.go")); !strings.Contains(string(b), "incomplete") {
		t.Errorf("preserved WIP is not visible to a continue_from retry")
	}

	// A second preserve with no further changes commits nothing (idempotent / no churn).
	if br2 := preserveFailedAttempt(ctx, wt); br2 != "" {
		t.Errorf("no-change preserve should return \"\", got %q", br2)
	}
}
