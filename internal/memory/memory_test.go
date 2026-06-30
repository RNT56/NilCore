package memory_test

import (
	"context"
	"fmt"
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
		// Distinct keys: (scope,project,mkey) is now the logical key, so a same-key
		// write would replace (upsert) rather than add a row. Five distinct keys ⇒
		// five rows, of which Context bounds the rendering to 2.
		_ = m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "p", Key: fmt.Sprintf("k%d", i), Value: "v"})
	}
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

// TestWriteReplacesSameKey proves the (scope,project,key) is a real key: re-writing
// the same key with a CHANGED value replaces the prior row rather than accumulating a
// stale duplicate, so recall never surfaces both the old and current value.
func TestWriteReplacesSameKey(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()
	if err := m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "p", Key: "task:7", Value: "first summary"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "p", Key: "task:7", Value: "revised summary"}); err != nil {
		t.Fatal(err)
	}
	got, err := m.Query(ctx, memory.ScopeProject, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("same-key re-write must replace, got %d rows: %+v", len(got), got)
	}
	if got[0].Value != "revised summary" {
		t.Errorf("expected current value, got %q", got[0].Value)
	}
}

// TestRememberDifferentValueReplaces is the recall-pollution regression: Remember of a
// same-key/different-value record (e.g. a re-run task with a changed summary) must end
// with exactly one row carrying the new value, not the old and new side by side.
func TestRememberDifferentValueReplaces(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()
	if _, err := m.Remember(ctx, []memory.Record{{Scope: memory.ScopeProject, Project: "p", Key: "task:7", Value: "old"}}); err != nil {
		t.Fatal(err)
	}
	n, err := m.Remember(ctx, []memory.Record{{Scope: memory.ScopeProject, Project: "p", Key: "task:7", Value: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed value is a new write, got %d", n)
	}
	got, _ := m.Query(ctx, memory.ScopeProject, "p", "task:7")
	if len(got) != 1 || got[0].Value != "new" {
		t.Errorf("recall must return only the current value, got %+v", got)
	}
}

// TestRememberBatchDedupesWithinBatch proves the hoisted dup set dedupes two identical
// records inside one batch (the set is updated as we write), and that an already-stored
// record is recognized as a dup without a per-record full-table read.
func TestRememberBatchDedupesWithinBatch(t *testing.T) {
	m := newMem(t)
	ctx := context.Background()
	if _, err := m.Remember(ctx, []memory.Record{{Scope: memory.ScopeProject, Project: "p", Key: "a", Value: "1"}}); err != nil {
		t.Fatal(err)
	}
	recs := []memory.Record{
		{Scope: memory.ScopeProject, Project: "p", Key: "a", Value: "1"}, // already stored → dup
		{Scope: memory.ScopeProject, Project: "p", Key: "b", Value: "2"}, // new
		{Scope: memory.ScopeProject, Project: "p", Key: "b", Value: "2"}, // dup within batch
	}
	n, err := m.Remember(ctx, recs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("batch should write exactly the one new distinct record, got %d", n)
	}
	all, _ := m.Query(ctx, memory.ScopeProject, "p", "")
	if len(all) != 2 {
		t.Errorf("expected 2 distinct keys stored, got %d: %+v", len(all), all)
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
