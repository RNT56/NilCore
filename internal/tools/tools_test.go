package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, tool Tool, workdir string, input string) (string, error) {
	t.Helper()
	return tool.Run(context.Background(), workdir, json.RawMessage(input))
}

func TestReadWriteEdit(t *testing.T) {
	dir := t.TempDir()

	if _, err := run(t, WriteTool{}, dir, `{"path":"sub/a.txt","content":"hello world"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := run(t, ReadTool{}, dir, `{"path":"sub/a.txt"}`)
	if err != nil || got != "hello world" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if _, err := run(t, EditTool{}, dir, `{"path":"sub/a.txt","old":"world","new":"nilcore"}`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, _ = run(t, ReadTool{}, dir, `{"path":"sub/a.txt"}`)
	if got != "hello nilcore" {
		t.Fatalf("after edit = %q", got)
	}
}

func TestEditUniqueness(t *testing.T) {
	dir := t.TempDir()
	_, _ = run(t, WriteTool{}, dir, `{"path":"a.txt","content":"x x x"}`)
	if _, err := run(t, EditTool{}, dir, `{"path":"a.txt","old":"x","new":"y"}`); err == nil {
		t.Error("expected error for non-unique old without all")
	}
	if _, err := run(t, EditTool{}, dir, `{"path":"a.txt","old":"x","new":"y","all":true}`); err != nil {
		t.Fatalf("edit all: %v", err)
	}
	got, _ := run(t, ReadTool{}, dir, `{"path":"a.txt"}`)
	if got != "y y y" {
		t.Errorf("got %q", got)
	}
}

func TestSearch(t *testing.T) {
	dir := t.TempDir()
	_, _ = run(t, WriteTool{}, dir, `{"path":"a.go","content":"package a\nfunc Foo() {}\n"}`)
	_, _ = run(t, WriteTool{}, dir, `{"path":"b.txt","content":"Foo here too\n"}`)

	got, err := run(t, SearchTool{}, dir, `{"pattern":"Foo","glob":"*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "a.go:2") || strings.Contains(got, "b.txt") {
		t.Errorf("search with glob = %q", got)
	}
	all, _ := run(t, SearchTool{}, dir, `{"pattern":"Foo"}`)
	if !strings.Contains(all, "a.go") || !strings.Contains(all, "b.txt") {
		t.Errorf("search all = %q", all)
	}
	none, _ := run(t, SearchTool{}, dir, `{"pattern":"zzz"}`)
	if none != "no matches" {
		t.Errorf("expected no matches, got %q", none)
	}
}

func TestPathEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, ReadTool{}, dir, `{"path":"../etc/passwd"}`); err == nil {
		t.Error("expected escape rejection for read")
	}
	if _, err := run(t, WriteTool{}, dir, `{"path":"../evil","content":"x"}`); err == nil {
		t.Error("expected escape rejection for write")
	}
}

func TestRegistry(t *testing.T) {
	r := Default()
	defs := r.Defs()
	if len(defs) != 5 {
		t.Fatalf("Defs = %d, want 5", len(defs))
	}
	if defs[0].Name != "read" {
		t.Errorf("first def = %q, want read", defs[0].Name)
	}
	if !r.Has("git") || r.Has("nope") {
		t.Error("Has mismatch")
	}
	if _, err := r.Dispatch(context.Background(), "nope", t.TempDir(), nil); err == nil {
		t.Error("expected error dispatching unknown tool")
	}
}

func TestGitTool(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	mustRun := func(args ...string) {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	mustRun("git", "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, GitTool{}, dir, `{"op":"add"}`); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := run(t, GitTool{}, dir, `{"op":"commit","message":"init"}`); err != nil {
		t.Fatalf("commit: %v", err)
	}
	logOut, err := run(t, GitTool{}, dir, `{"op":"log"}`)
	if err != nil || !strings.Contains(logOut, "init") {
		t.Fatalf("log = %q, %v", logOut, err)
	}
	if _, err := run(t, GitTool{}, dir, `{"op":"commit"}`); err == nil {
		t.Error("commit without message should fail")
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An in-tree symlink that points OUTSIDE the worktree.
	if err := os.Symlink(outside, filepath.Join(dir, "leak")); err != nil {
		t.Skip("symlinks unsupported on this platform")
	}
	// A symlink whose target is a specific outside file.
	if err := os.Symlink(secret, filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}

	// Reading/writing THROUGH the symlinked dir must be rejected.
	if _, err := run(t, ReadTool{}, dir, `{"path":"leak/secret.txt"}`); err == nil {
		t.Error("read via an in-tree symlink to outside must be rejected")
	}
	if _, err := run(t, WriteTool{}, dir, `{"path":"leak/pwn.txt","content":"x"}`); err == nil {
		t.Error("write via an in-tree symlink to outside must be rejected")
	}
	// Reading the symlink file itself (points out) must be rejected.
	if _, err := run(t, ReadTool{}, dir, `{"path":"evil"}`); err == nil {
		t.Error("reading a symlink that points outside must be rejected")
	}
	// Editing through the symlink must be rejected (and never touch the secret).
	_, _ = run(t, EditTool{}, dir, `{"path":"evil","old":"top secret","new":"pwned"}`)
	if b, _ := os.ReadFile(secret); string(b) != "top secret" {
		t.Fatal("confinement breached: the outside file was modified")
	}

	// Regression: a legit nested NEW file under the worktree still works.
	if _, err := run(t, WriteTool{}, dir, `{"path":"sub/ok.txt","content":"fine"}`); err != nil {
		t.Errorf("legit nested write should still succeed: %v", err)
	}
	if got, _ := run(t, ReadTool{}, dir, `{"path":"sub/ok.txt"}`); got != "fine" {
		t.Errorf("legit read round-trip = %q", got)
	}
}
