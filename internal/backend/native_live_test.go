package backend

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLiveReindex verifies the P3-T16 incremental-index signal mapping: which
// structured file ops UPDATE (re-index) a path and which REMOVE (prune) it. This is
// the fix that makes a deleted/renamed-away file actually prune from the live graph —
// previously only write/edit ever signalled the index, so a `patch` delete/move left
// the removed file's nodes and dangling incoming edges behind.
func TestLiveReindex(t *testing.T) {
	dir := "/work"
	abs := func(p string) string { return filepath.Join(dir, p) }

	cases := []struct {
		name       string
		tool       string
		input      string
		wantUpdate []string
		wantRemove []string
	}{
		{
			name:       "write re-indexes the file",
			tool:       "write",
			input:      `{"path":"a.go","content":"x"}`,
			wantUpdate: []string{abs("a.go")},
		},
		{
			name:       "edit re-indexes the file",
			tool:       "edit",
			input:      `{"path":"pkg/b.go"}`,
			wantUpdate: []string{abs("pkg/b.go")},
		},
		{
			name:       "absolute path is passed through unchanged",
			tool:       "write",
			input:      `{"path":"/abs/c.go"}`,
			wantUpdate: []string{"/abs/c.go"},
		},
		{
			name:       "patch delete_file prunes the removed file",
			tool:       "patch",
			input:      `{"ops":[{"kind":"delete_file","path":"gone.go"}]}`,
			wantRemove: []string{abs("gone.go")},
		},
		{
			name:       "patch move_to prunes old path and indexes new",
			tool:       "patch",
			input:      `{"ops":[{"kind":"update_file","path":"old.go","move_to":"new.go"}]}`,
			wantUpdate: []string{abs("new.go")},
			wantRemove: []string{abs("old.go")},
		},
		{
			name:       "patch update_file in place re-indexes",
			tool:       "patch",
			input:      `{"ops":[{"kind":"update_file","path":"u.go"}]}`,
			wantUpdate: []string{abs("u.go")},
		},
		{
			name:       "patch add_file re-indexes",
			tool:       "patch",
			input:      `{"ops":[{"kind":"add_file","path":"n.go"}]}`,
			wantUpdate: []string{abs("n.go")},
		},
		{
			name:       "patch mixed ops signal each op",
			tool:       "patch",
			input:      `{"ops":[{"kind":"add_file","path":"add.go"},{"kind":"delete_file","path":"del.go"},{"kind":"update_file","path":"mv.go","move_to":"mvd.go"}]}`,
			wantUpdate: []string{abs("add.go"), abs("mvd.go")},
			// del.go is deleted; mv.go's move_to prunes the source path too.
			wantRemove: []string{abs("del.go"), abs("mv.go")},
		},
		{
			name:  "unrelated tool signals nothing",
			tool:  "search",
			input: `{"query":"x"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotUpdate, gotRemove []string
			update := func(_ context.Context, p string) { gotUpdate = append(gotUpdate, p) }
			remove := func(_ context.Context, p string) { gotRemove = append(gotRemove, p) }

			liveReindex(context.Background(), tc.tool, dir, json.RawMessage(tc.input), update, remove)

			if !reflect.DeepEqual(gotUpdate, tc.wantUpdate) {
				t.Errorf("update paths = %v, want %v", gotUpdate, tc.wantUpdate)
			}
			if !reflect.DeepEqual(gotRemove, tc.wantRemove) {
				t.Errorf("remove paths = %v, want %v", gotRemove, tc.wantRemove)
			}
		})
	}
}

// TestLiveReindexNilHooks confirms the hooks are independently nil-gated: a session
// that wires update but not remove (or neither) must not panic on a removing op.
func TestLiveReindexNilHooks(t *testing.T) {
	// remove nil, delete_file op: must be a no-op, not a nil-call panic.
	liveReindex(context.Background(), "patch", "/w",
		json.RawMessage(`{"ops":[{"kind":"delete_file","path":"x.go"}]}`), nil, nil)

	var updated []string
	update := func(_ context.Context, p string) { updated = append(updated, p) }
	// update wired, remove nil, move_to op: the destination is still indexed; the
	// source prune is silently skipped (no remove sink).
	liveReindex(context.Background(), "patch", "/w",
		json.RawMessage(`{"ops":[{"kind":"update_file","path":"o.go","move_to":"n.go"}]}`), update, nil)
	if want := []string{filepath.Join("/w", "n.go")}; !reflect.DeepEqual(updated, want) {
		t.Fatalf("update paths = %v, want %v", updated, want)
	}
}
