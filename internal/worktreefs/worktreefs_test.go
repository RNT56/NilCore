package worktreefs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// resolveDir returns a temp dir with symlinks resolved (macOS /var → /private/var)
// so confine()'s lexical containment check lines up with EvalSymlinks output.
func resolveDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return p
}

// TestConfine is the named gate (Verify line): SafeJoin must accept legitimate
// in-tree paths and reject every escape — upward traversal, absolute paths, and
// in-tree symlinks that point out.
func TestConfine(t *testing.T) {
	root := resolveDir(t)

	t.Run("accepts forward-only relative path", func(t *testing.T) {
		got, err := SafeJoin(root, ".nilcore/artifacts/x.json")
		if err != nil {
			t.Fatalf("SafeJoin rejected a legitimate nested path: %v", err)
		}
		want := filepath.Join(root, ".nilcore", "artifacts", "x.json")
		if got != want {
			t.Errorf("SafeJoin = %q, want %q", got, want)
		}
	})

	t.Run("rejects empty", func(t *testing.T) {
		if _, err := SafeJoin(root, ""); err == nil {
			t.Error("empty rel must be rejected")
		}
	})

	t.Run("rejects dotdot that escapes root", func(t *testing.T) {
		for _, rel := range []string{"..", "../escape", "a/../../escape", "a/b/../../../escape"} {
			if _, err := SafeJoin(root, rel); err == nil {
				t.Errorf("SafeJoin(%q) must reject an escape via ..", rel)
			}
		}
	})

	t.Run("allows dotdot that stays inside root", func(t *testing.T) {
		// "a/../b" cleans to "b", still inside root — must be accepted.
		got, err := SafeJoin(root, "a/../b.json")
		if err != nil {
			t.Fatalf("an in-root .. must be allowed: %v", err)
		}
		if got != filepath.Join(root, "b.json") {
			t.Errorf("SafeJoin in-root .. = %q, want %q", got, filepath.Join(root, "b.json"))
		}
	})

	t.Run("absolute rel joins harmlessly inside root", func(t *testing.T) {
		// Behavior-preserving: an absolute path is joined onto root (filepath.Join
		// drops the leading separator), landing a shadow file INSIDE root rather than
		// at the host absolute path — so it never touches the real outside file.
		got, err := SafeJoin(root, "/etc/passwd")
		if err != nil {
			t.Fatalf("absolute rel must join inside root, not be rejected: %v", err)
		}
		if got != filepath.Join(root, "etc", "passwd") {
			t.Errorf("SafeJoin(/etc/passwd) = %q, want it joined inside root %q", got, filepath.Join(root, "etc", "passwd"))
		}
	})

	t.Run("rejects in-tree symlink escape", func(t *testing.T) {
		outside := resolveDir(t)
		link := filepath.Join(root, "evil")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		// A path THROUGH the symlinked dir resolves outside root and must be refused.
		if _, err := SafeJoin(root, "evil/secret.txt"); err == nil {
			t.Error("a path through an in-tree symlink that escapes root must be rejected")
		}
	})
}

// TestConfineSafeAbs covers the absolute read-root counterpart.
func TestConfineSafeAbs(t *testing.T) {
	root := resolveDir(t)
	inside := filepath.Join(root, "file.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeAbs(root, inside); err != nil {
		t.Errorf("an absolute path inside root must be accepted: %v", err)
	}
	if _, err := SafeAbs(root, "/etc/passwd"); err == nil {
		t.Error("an absolute path outside root must be rejected")
	}
}

// TestWriteAtomicRoundTrip: a write lands at the confined path and reads back
// byte-identical; an explicit perm is honored.
func TestWriteAtomicRoundTrip(t *testing.T) {
	root := resolveDir(t)
	body := []byte("hello evidence")
	if err := WriteAtomic(root, ".nilcore/artifacts/id.json", body, 0o600); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	p := filepath.Join(root, ".nilcore", "artifacts", "id.json")
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-trip = %q, want %q", got, body)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
	}
}

// TestWriteAtomicRejectsEscape: WriteAtomic confines through SafeJoin, so an
// escaping rel never writes a byte.
func TestWriteAtomicRejectsEscape(t *testing.T) {
	root := resolveDir(t)
	outside := resolveDir(t)
	target := filepath.Join(outside, "escaped.txt")
	if err := WriteAtomic(root, "../"+filepath.Base(outside)+"/escaped.txt", []byte("x"), 0o644); err == nil {
		t.Error("WriteAtomic must reject an escaping rel")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("no escape file may exist")
	}
}

// TestWriteAtomicSymlinkPlantedAtTarget: a symlink planted at the destination that
// points outside root is refused at the SafeJoin confinement step (its EvalSymlinks
// resolves outside root), so WriteAtomic fails closed and the secret is untouched.
func TestWriteAtomicSymlinkPlantedAtTarget(t *testing.T) {
	root := resolveDir(t)
	outside := resolveDir(t)
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink directly at the target name, pointing at the secret.
	if err := os.MkdirAll(filepath.Join(root, ".nilcore", "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".nilcore", "artifacts", "id.json")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := WriteAtomic(root, ".nilcore/artifacts/id.json", []byte("NEW"), 0o644); err == nil {
		t.Fatal("WriteAtomic must refuse a target whose symlink escapes root")
	}
	got, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatal("confinement breached: the outside secret was overwritten through a symlink")
	}
}

// TestOpenNoFollow: opening a planted symlink fails closed (ELOOP), so a read or
// write through a swapped-in link is refused at the final component.
func TestOpenNoFollow(t *testing.T) {
	root := resolveDir(t)
	outside := resolveDir(t)
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	f, err := OpenNoFollow(link, os.O_RDONLY, 0)
	if err == nil {
		f.Close()
		t.Fatal("OpenNoFollow must refuse a symlink (O_NOFOLLOW)")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Logf("note: error was %v (expected ELOOP); open still failed closed", err)
	}

	// Sanity: a plain regular file opens fine.
	plain := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(plain, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := OpenNoFollow(plain, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenNoFollow on a regular file failed: %v", err)
	}
	g.Close()
}
