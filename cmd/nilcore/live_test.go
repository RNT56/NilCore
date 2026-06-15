package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveSession seeds a worktree graph, fuses memory, and stays worktree-aware: an
// incremental edit is reflected by the next query without a full re-index.
func TestLiveSession(t *testing.T) {
	dir := t.TempDir()
	src := "package p\nfunc leaf() int { return 1 }\nfunc Run() int { return leaf() }\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	update, query, closeFn := liveSession(nil, "proj")(dir) // nil memory: graph-only
	if update == nil || query == nil || closeFn == nil {
		t.Fatal("live session did not open")
	}
	defer closeFn()

	ctx := context.Background()
	// Run calls leaf — the initial index sees that edge.
	if out := query(ctx, "Run"); !strings.Contains(out, "leaf") {
		t.Errorf("live query for Run should surface its callee leaf:\n%s", out)
	}

	// An incremental edit (a new caller of Run) is reflected by the next query —
	// worktree-aware, no full re-index.
	p2 := filepath.Join(dir, "q.go")
	if err := os.WriteFile(p2, []byte("package p\nfunc helper() int { return Run() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	update(ctx, p2)
	if out := query(ctx, "Run"); !strings.Contains(out, "helper") {
		t.Errorf("after the edit, live query for Run should surface its new caller helper:\n%s", out)
	}

	// An unknown symbol renders the empty form, not a crash.
	if out := query(ctx, "Nonexistent"); !strings.Contains(out, "no live facts") {
		t.Errorf("unknown symbol = %q", out)
	}
}
