package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a throwaway git repo with one commit so HEAD exists.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return repo
}

func TestCreateAndCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "P1-T01")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := os.Stat(wt.Path()); err != nil {
		t.Fatalf("worktree path missing: %v", err)
	}
	if wt.Branch() != "task/P1-T01" {
		t.Errorf("Branch() = %q, want task/P1-T01", wt.Branch())
	}
	// It must be a file inside the worktree (checked-out HEAD has a .git file).
	if _, err := os.Stat(filepath.Join(wt.Path(), ".git")); err != nil {
		t.Errorf("worktree .git missing: %v", err)
	}

	list := gitOut(t, repo, "worktree", "list")
	if !strings.Contains(list, wt.Path()) {
		t.Fatalf("worktree not registered:\n%s", list)
	}

	if err := wt.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(wt.Path()); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists after cleanup (err=%v)", err)
	}
	// Branch should be gone.
	if branches := gitOut(t, repo, "branch", "--list", "task/P1-T01"); strings.TrimSpace(branches) != "" {
		t.Errorf("branch not deleted: %q", branches)
	}
	// Idempotent.
	if err := wt.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}

func TestCreateEmptyTaskID(t *testing.T) {
	if _, err := Create(context.Background(), t.TempDir(), ""); err == nil {
		t.Fatal("expected error for empty task id")
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
