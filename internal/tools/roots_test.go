package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveDir returns a temp dir with symlinks resolved (macOS /var → /private/var),
// matching what the wiring does when it registers a read root: roots and the paths
// addressed under them are symlink-resolved, so confine()'s lexical check lines up.
func resolveDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return p
}

// An added read root is readable by ABSOLUTE path; a relative path still resolves
// only against the worktree (so a bare name can never reach the extra root, and
// "../escape" is still rejected).
func TestReadRootAbsoluteAllowed(t *testing.T) {
	work := resolveDir(t)
	root := resolveDir(t)
	libFile := filepath.Join(root, "lib", "util.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib // hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := ReadTool{ReadRoots: []string{root}}
	out, err := run(t, rt, work, `{"path":`+quote(libFile)+`}`)
	if err != nil {
		t.Fatalf("read added root by absolute path: %v", err)
	}
	if out != "package lib // hello" {
		t.Errorf("read root file = %q", out)
	}
}

// The core I4 guarantee: an absolute path outside the worktree AND every added root
// is rejected. A worktree-only ReadTool rejects all absolute escapes.
func TestReadRootRejectsOutside(t *testing.T) {
	work := resolveDir(t)
	root := resolveDir(t)
	other := resolveDir(t) // a dir that was NOT added

	otherFile := filepath.Join(other, "secret.txt")
	if err := os.WriteFile(otherFile, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := ReadTool{ReadRoots: []string{root}} // only `root` is added, not `other`
	if _, err := run(t, rt, work, `{"path":`+quote(otherFile)+`}`); err == nil {
		t.Error("reading an un-added absolute path must be rejected")
	}
	if _, err := run(t, rt, work, `{"path":"/etc/passwd"}`); err == nil {
		t.Error("reading /etc/passwd must be rejected")
	}
	// With no roots at all, every absolute escape is rejected too.
	if _, err := run(t, ReadTool{}, work, `{"path":`+quote(otherFile)+`}`); err == nil {
		t.Error("worktree-only ReadTool must reject an absolute escape")
	}
}

func TestReadRelativeStaysWorktree(t *testing.T) {
	work := resolveDir(t)
	rt := ReadTool{ReadRoots: []string{resolveDir(t)}}
	// A relative escape is rejected regardless of added roots (relative = worktree).
	if _, err := run(t, rt, work, `{"path":"../etc/passwd"}`); err == nil {
		t.Error("relative ../escape must still be rejected with roots present")
	}
}

// A symlink inside an added root that points outside it must not let a read escape
// (the same symlink defense the worktree has, applied per-root).
func TestReadRootSymlinkEscapeRejected(t *testing.T) {
	work := resolveDir(t)
	root := resolveDir(t)
	secret := resolveDir(t)
	if err := os.WriteFile(filepath.Join(secret, "k"), []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	rt := ReadTool{ReadRoots: []string{root}}
	if _, err := run(t, rt, work, `{"path":`+quote(filepath.Join(link, "k"))+`}`); err == nil {
		t.Error("reading through an in-root symlink that escapes the root must be rejected")
	}
}

// WriteTool/EditTool have NO ReadRoots and resolve only against the worktree, so an
// extra root is never writable: a write addressed at the root's absolute path lands
// INSIDE the worktree (the abs path is joined onto workdir), never in the root.
func TestExtraRootNeverWritable(t *testing.T) {
	work := resolveDir(t)
	root := resolveDir(t)
	target := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// WriteTool only takes (workdir, path); there is no field to pass roots, so it
	// cannot even be configured to write the root. Writing the root's absolute path
	// writes a shadow file under the worktree, leaving the real root file untouched.
	if _, err := run(t, WriteTool{}, work, `{"path":`+quote(target)+`,"content":"HACKED"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("extra-root file was modified by WriteTool: %q — extra roots must be read-only", got)
	}
}

// Search spans the worktree and the added roots: worktree matches are relative,
// root matches are absolute (so the model can read them back).
func TestSearchSpansRoots(t *testing.T) {
	work := resolveDir(t)
	root := resolveDir(t)
	if err := os.WriteFile(filepath.Join(work, "a.go"), []byte("NEEDLE in worktree"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(root, "b.go")
	if err := os.WriteFile(rootFile, []byte("NEEDLE in root"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := SearchTool{ReadRoots: []string{root}}
	out, err := run(t, st, work, `{"pattern":"NEEDLE"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "a.go:1:") {
		t.Errorf("worktree match missing or not relative: %q", out)
	}
	if !strings.Contains(out, rootFile+":1:") {
		t.Errorf("root match missing or not absolute: %q", out)
	}
}

func quote(s string) string { return `"` + s + `"` }
