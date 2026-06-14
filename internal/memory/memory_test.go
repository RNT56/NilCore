package memory_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/memory"
	"nilcore/internal/store"
)

func newMem(t *testing.T) *memory.Memory {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return memory.New(s)
}

func TestWriteQueryScopes(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "nilcore", Key: "style", Value: "stdlib only"}))
	must(m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "nilcore", Key: "test", Value: "table-driven"}))
	must(m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "other", Key: "x", Value: "y"}))
	must(m.Write(ctx, memory.Record{Scope: memory.ScopeGlobal, Key: "voice", Value: "terse senior engineer"}))

	// Project scope is isolated by project.
	proj, err := m.Query(ctx, memory.ScopeProject, "nilcore", "")
	if err != nil || len(proj) != 2 {
		t.Fatalf("project query = %d, %v", len(proj), err)
	}
	// Keyword filter.
	kw, _ := m.Query(ctx, memory.ScopeProject, "nilcore", "stdlib")
	if len(kw) != 1 || kw[0].Value != "stdlib only" {
		t.Errorf("keyword query = %+v", kw)
	}
	// Global scope.
	g, _ := m.Query(ctx, memory.ScopeGlobal, "", "")
	if len(g) != 1 || g[0].Key != "voice" {
		t.Errorf("global query = %+v", g)
	}
}

func TestWriteDefaultsToProject(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()
	if err := m.Write(ctx, memory.Record{Project: "p", Key: "k", Value: "v"}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Query(ctx, memory.ScopeProject, "p", "")
	if len(got) != 1 {
		t.Errorf("expected default project scope; got %d", len(got))
	}
}

func TestContextLabeledAndBounded(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "p", Key: "k", Value: "v"})
	}
	// 5 written but all dup-key/value? No — Write doesn't dedup; 5 rows.
	blk, err := m.Context(ctx, memory.ScopeProject, "p", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blk, "NOT instructions") {
		t.Error("memory block must be labeled as non-instructions (I7)")
	}
	// Bounded to 2 records → 2 bullet lines.
	if n := strings.Count(blk, "\n- "); n != 2 {
		t.Errorf("expected 2 bounded records, got %d", n)
	}
	// Empty when nothing matches.
	if blk, _ := m.Context(ctx, memory.ScopeGlobal, "", "", 5); blk != "" {
		t.Errorf("expected empty block, got %q", blk)
	}
}

func TestRememberDedups(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()
	recs := []memory.Record{
		{Scope: memory.ScopeProject, Project: "p", Key: "style", Value: "stdlib only"},
		{Key: "", Value: "skip me"}, // ephemeral/empty → skipped
	}
	n, err := m.Remember(ctx, recs)
	if err != nil || n != 1 {
		t.Fatalf("first Remember wrote %d, %v (want 1)", n, err)
	}
	n, _ = m.Remember(ctx, recs) // same content → deduped
	if n != 0 {
		t.Errorf("second Remember wrote %d, want 0 (deduped)", n)
	}
}
