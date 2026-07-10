package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// TestSearchDoesNotFollowSymlinks proves an in-tree symlink pointing OUT of the
// worktree does not leak the target's contents to the model (I4). filepath.WalkDir
// yields the symlink as a non-dir entry; os.ReadFile would follow it, so searchRoot
// must skip symlinks (matching ReadTool's OpenNoFollow hardening).
func TestSearchDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	// A secret living OUTSIDE the worktree.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET_NEEDLE_VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An in-tree symlink pointing at it.
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := run(t, SearchTool{}, dir, `{"pattern":"SECRET_NEEDLE_VALUE"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "SECRET_NEEDLE_VALUE") {
		t.Errorf("search followed an in-tree symlink and leaked out-of-worktree content: %q", got)
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

// TestGitShowRejectsLeadingDashRev is the option-injection regression for safeRev: a
// rev beginning with '-' (e.g. "--ext-diff") must be rejected BEFORE it reaches
// `git show --stat <rev>`, so a model-supplied rev can never be read as a git option.
// The rejection is pure-regex (it fires before exec), so this needs no git binary.
func TestGitShowRejectsLeadingDashRev(t *testing.T) {
	dir := t.TempDir()
	for _, rev := range []string{"--ext-diff", "-c", "--output=/tmp/x", "-HEAD"} {
		if _, err := run(t, GitTool{}, dir, `{"op":"show","rev":"`+rev+`"}`); err == nil {
			t.Errorf("show must reject leading-dash rev %q as an option injection", rev)
		}
	}
	// A legit rev must still pass the character validator. It may fail later (no repo /
	// git absent), but never with the "disallowed characters" rejection.
	if _, err := run(t, GitTool{}, dir, `{"op":"show","rev":"HEAD~1"}`); err != nil &&
		strings.Contains(err.Error(), "disallowed characters") {
		t.Errorf("legit rev HEAD~1 wrongly rejected by safeRev: %v", err)
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

// TestWriteIsAtomicNewInode proves overwrites never truncate-in-place: an atomic
// temp+rename produces a NEW inode for the destination, whereas an in-place
// O_TRUNC write would keep the same inode (and would be observable as truncated
// mid-write). A changed inode is the structural signature of rename-over-write.
func TestWriteIsAtomicNewInode(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, WriteTool{}, dir, `{"path":"a.txt","content":"first version"}`); err != nil {
		t.Fatalf("write1: %v", err)
	}
	p := filepath.Join(dir, "a.txt")
	ino1 := inodeOf(t, p)

	if _, err := run(t, WriteTool{}, dir, `{"path":"a.txt","content":"second, longer, version of the file"}`); err != nil {
		t.Fatalf("write2: %v", err)
	}
	ino2 := inodeOf(t, p)
	if ino1 == ino2 {
		t.Fatalf("inode unchanged across overwrite (%d): write truncated in place, not atomic", ino1)
	}
	if got, _ := run(t, ReadTool{}, dir, `{"path":"a.txt"}`); got != "second, longer, version of the file" {
		t.Fatalf("after atomic overwrite = %q", got)
	}

	// edit must also be atomic (it routes through the same writeNoFollow).
	if _, err := run(t, EditTool{}, dir, `{"path":"a.txt","old":"second","new":"third"}`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if inodeOf(t, p) == ino2 {
		t.Fatal("edit kept the same inode: edit truncated in place, not atomic")
	}
}

// TestWriteNoPartialContentObservable hammers a file with overwrites from one
// goroutine while another goroutine reads it concurrently. Because the write is a
// temp+rename, every read must observe a COMPLETE prior version — never an empty
// or truncated file. An in-place O_TRUNC write would let a reader catch the file
// after truncation but before the bytes land. Run with -race.
func TestWriteNoPartialContentObservable(t *testing.T) {
	dir := t.TempDir()
	const small = "short content"
	const large = "a much, much longer content string that would be observably truncated mid-write if writes were not atomic — repeated to be big enough to matter: " +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	// Seed so the path always exists for the reader.
	if _, err := run(t, WriteTool{}, dir, `{"path":"hot.txt","content":"`+small+`"}`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := filepath.Join(dir, "hot.txt")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // writer: alternate small/large bodies until told to stop
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			body := small
			if i%2 == 0 {
				body = large
			}
			if err := writeNoFollow(dir, p, []byte(body)); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
		}
	}()

	// reader: every observed content must be a COMPLETE version, never torn.
	for i := 0; i < 4000; i++ {
		b, err := os.ReadFile(p)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d: %v", i, err)
		}
		if s := string(b); s != small && s != large {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d observed a PARTIAL/torn file (len=%d): atomicity broken", i, len(b))
		}
	}
	close(stop)
	wg.Wait()
}

// TestWriteNoTempFileLeak asserts the temp file used for the atomic rename does
// not linger after a successful write (no .nilcore-tmp-* droppings).
func TestWriteNoTempFileLeak(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if _, err := run(t, WriteTool{}, dir, `{"path":"f.txt","content":"v"}`); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nilcore-tmp-") {
			t.Fatalf("leaked temp file: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "f.txt" {
		t.Fatalf("unexpected dir contents after atomic writes: %v", entries)
	}
}

func inodeOf(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("inode introspection unsupported on this platform")
	}
	return uint64(st.Ino)
}

func TestGitDiffArgInjectionNeutralized(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	c := exec.Command("git", "init", "-q")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	// A path that would be a flag if `--` weren't inserted: --output writes the
	// diff to an attacker-named file. With `--` it's an (unmatched) pathspec.
	target := filepath.Join(dir, "EXFIL")
	inj := `{"op":"diff","paths":["--output=` + target + `"]}`
	_, _ = run(t, GitTool{}, dir, inj)
	if _, err := os.Stat(target); err == nil {
		t.Fatal("git diff treated a model path as --output: arg injection not neutralized")
	}
}

func TestGitHooksNeutralized(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	c := exec.Command("git", "init", "-q")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	// A model can write into .git/hooks; a pre-commit hook must NOT run on commit.
	marker := filepath.Join(dir, "HOOK_RAN")
	hook := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, GitTool{}, dir, `{"op":"add"}`); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := run(t, GitTool{}, dir, `{"op":"commit","message":"x"}`); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pre-commit hook executed: core.hooksPath clamp not effective")
	}
}
