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
