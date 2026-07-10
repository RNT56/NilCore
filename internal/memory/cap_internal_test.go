package memory

// Internal test (package memory) so it can reference the unexported taskMemoryCap /
// taskKeyPrefix. It proves the unbounded-growth cap is WIRED on the Write hot path,
// not merely available on the store.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/store"
)

// TestWriteCapsTaskFamily is the unbounded-growth regression: memWriteBack appends one
// task:<id> row per verified task with a UNIQUE key, so without a cap the project
// partition grows forever. Writing more than taskMemoryCap task rows must leave the
// partition bounded to the cap, keep the NEWEST, drop the oldest, and never touch a
// non-task key.
func TestWriteCapsTaskFamily(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m := New(s)
	ctx := context.Background()

	// A non-task key that must survive the task:* prune untouched.
	if err := m.Write(ctx, Record{Scope: ScopeProject, Project: "p", Key: "style", Value: "stdlib only"}); err != nil {
		t.Fatal(err)
	}
	// Write cap + overflow distinct task rows through the real Write path.
	const overflow = 5
	total := taskMemoryCap + overflow
	for i := 0; i < total; i++ {
		if err := m.Write(ctx, Record{Scope: ScopeProject, Project: "p", Key: fmt.Sprintf("task:%d", i), Value: fmt.Sprintf("v%d", i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	all, err := m.Query(ctx, ScopeProject, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	taskRows, haveStyle := 0, false
	seen := map[string]bool{}
	for _, r := range all {
		seen[r.Key] = true
		if strings.HasPrefix(r.Key, taskKeyPrefix) {
			taskRows++
		}
		if r.Key == "style" {
			haveStyle = true
		}
	}
	if taskRows != taskMemoryCap {
		t.Errorf("task:* rows = %d, want the cap %d (partition must stay bounded)", taskRows, taskMemoryCap)
	}
	if !haveStyle {
		t.Error("prune must not touch a non-task key (style was dropped)")
	}
	// Newest survive; the oldest overflow rows aged out.
	if !seen[fmt.Sprintf("task:%d", total-1)] {
		t.Errorf("newest task row task:%d must survive the cap", total-1)
	}
	for i := 0; i < overflow; i++ {
		if seen[fmt.Sprintf("task:%d", i)] {
			t.Errorf("oldest task row task:%d must be pruned", i)
		}
	}
}
