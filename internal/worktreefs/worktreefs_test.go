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

// TestReadConfinedRoundTrip: ReadConfined returns a confined regular file's bytes
// verbatim, and rejects a directory.
func TestReadConfinedRoundTrip(t *testing.T) {
	root := resolveDir(t)
	p := filepath.Join(root, "file.txt")
	if err := os.WriteFile(p, []byte("hello confined read"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfined(p)
	if err != nil {
		t.Fatalf("ReadConfined: %v", err)
	}
	if string(got) != "hello confined read" {
		t.Errorf("ReadConfined = %q, want the file contents", got)
	}
	// A directory must be refused (not read as bytes).
	if _, err := ReadConfined(root); err == nil {
		t.Error("ReadConfined must reject a directory")
	}
}

// TestReadConfinedRefusesFinalComponentSymlink is the adversarial regression for the
// read-side TOCTOU: a sandboxed process swaps the final path component for a symlink
// pointing at an out-of-worktree secret AFTER the confinement check. A plain
// os.ReadFile would follow it and leak the secret; ReadConfined opens with
// O_NOFOLLOW and must refuse — the secret's bytes must never come back.
func TestReadConfinedRefusesFinalComponentSymlink(t *testing.T) {
	root := resolveDir(t)
	outside := resolveDir(t)
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Swap the final component in-tree for a symlink to the outside secret.
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := ReadConfined(link)
	if err == nil {
		t.Fatalf("ReadConfined must refuse a final-component symlink; got %q", got)
	}
	if string(got) == "TOP SECRET" {
		t.Fatal("confinement breached: the outside secret leaked through a symlink read")
	}
}

// TestWriteConfinedRefusesParentSymlinkSwap is the regression for the parent-dir
// TOCTOU (mkdirAllNoFollow): a sandboxed process swaps an existing PARENT directory
// component for a symlink pointing outside the worktree after the confinement check.
// os.MkdirAll would traverse the symlink and land the write outside; the stepwise
// no-follow mkdir must refuse, and nothing may be written under the outside target.
func TestWriteConfinedRefusesParentSymlinkSwap(t *testing.T) {
	root := resolveDir(t)
	outside := resolveDir(t)
	// The attacker's out-of-tree directory the parent symlink points at.
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// In-tree, "sub" is a symlink to the outside dir (the swapped parent). A write to
	// root/sub/inner/file.txt must NOT create outside/inner/file.txt.
	link := filepath.Join(root, "sub")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	target := filepath.Join(root, "sub", "inner", "file.txt")
	// The swapped "sub" parent is BELOW root — an in-worktree component the sandboxed
	// process could plant. Bounding the no-follow check to root must still REFUSE it.
	if err := WriteConfined(root, target, []byte("ESCAPED"), 0o644); err == nil {
		t.Fatal("WriteConfined must refuse to traverse a symlinked parent component")
	}
	// The escape file must not exist under the outside directory.
	if _, err := os.Stat(filepath.Join(outside, "inner", "file.txt")); err == nil {
		t.Fatal("confinement breached: a file was created through a symlinked parent")
	}
}

// TestWriteConfinedThroughSymlinkedAncestor is the regression for the macOS
// `/var`→`/private/var` case that this fix restores: the worktree root's ABSOLUTE
// path legitimately contains a symlinked ANCESTOR (the host's own stable filesystem,
// e.g. a symlinked $TMPDIR), which is NOT attacker-writable in the sandbox threat
// model. The no-follow parent-dir check must be BOUNDED to the root: a symlink ABOVE
// the confinement boundary is trusted, so a write to a fresh nested path under such a
// root must SUCCEED — the previous unbounded walk-from-filesystem-root wrongly rejected
// it with `refusing to traverse symlinked path component`.
func TestWriteConfinedThroughSymlinkedAncestor(t *testing.T) {
	base := resolveDir(t)
	// realParent is a real directory; linkParent is a symlink to it. Using linkParent
	// as an ANCESTOR of the worktree root reproduces `/var`→`/private/var`: the root's
	// path string contains a symlinked component, but it is above the boundary.
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// The worktree root, addressed THROUGH the symlinked ancestor.
	rootViaLink := filepath.Join(linkParent, "worktree")
	if err := os.Mkdir(rootViaLink, 0o755); err != nil {
		t.Fatal(err)
	}

	// mkdirAllNoFollow must not reject the symlinked ancestor above the root.
	target := filepath.Join(rootViaLink, ".nilcore", "artifacts", "id.json")
	if err := mkdirAllNoFollow(rootViaLink, filepath.Dir(target)); err != nil {
		t.Fatalf("mkdirAllNoFollow must trust a symlinked ANCESTOR of the root: %v", err)
	}

	// The full WriteConfined path (mkdir + atomic temp+rename) must SUCCEED, and the
	// bytes must land — resolving through the symlinked ancestor — byte-identical.
	body := []byte("evidence written under a symlinked-ancestor root")
	if err := WriteConfined(rootViaLink, target, body, 0o644); err != nil {
		t.Fatalf("WriteConfined must succeed under a symlinked-ancestor root: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back through symlinked ancestor: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-trip = %q, want %q", got, body)
	}
	// And it must be the SAME real file whether addressed via the link or the real path.
	viaReal := filepath.Join(realParent, "worktree", ".nilcore", "artifacts", "id.json")
	got2, err := os.ReadFile(viaReal)
	if err != nil {
		t.Fatalf("read back via the real (de-symlinked) path: %v", err)
	}
	if string(got2) != string(body) {
		t.Errorf("de-symlinked read = %q, want %q — the write landed off the real path", got2, body)
	}
}

// symlinkedHostAncestorRoot builds a worktree root that is reached THROUGH a symlinked
// host ancestor — the macOS `/var`→`/private/var` shape — and returns BOTH the raw
// (through-the-link) root path and the resolved (real) root path. Callers pass the raw
// root to reproduce evverify's `Root = box.Workdir()`, which stores the path RAW.
func symlinkedHostAncestorRoot(t *testing.T) (rawRoot, realRoot string) {
	t.Helper()
	base := resolveDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link") // symlinked host ancestor (above root)
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	rawRoot = filepath.Join(linkParent, "worktree") // addressed through the symlink
	if err := os.Mkdir(rawRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return rawRoot, filepath.Join(realParent, "worktree")
}

// TestWriteConfinedRawRootThroughSymlinkedHostAncestor is the mandated macOS-style
// case: a worktree root reached THROUGH a symlinked host ancestor, with the root passed
// RAW (uncanonicalized, like evverify's box.Workdir()). A write to a fresh nested path
// under such a root must SUCCEED and land at the REAL (de-symlinked) file. This is the
// legitimate side of the boundary-derivation fix: the host-ancestor symlink is resolved
// on BOTH sides (canonRoot and canonClean), so they agree and the tail is the real
// in-worktree suffix — no spurious escape at the Rel() step.
func TestWriteConfinedRawRootThroughSymlinkedHostAncestor(t *testing.T) {
	rawRoot, realRoot := symlinkedHostAncestorRoot(t)

	target := filepath.Join(rawRoot, ".nilcore", "artifacts", "id.json")
	body := []byte("evidence under a RAW symlinked-host-ancestor root")
	if err := WriteConfined(rawRoot, target, body, 0o644); err != nil {
		t.Fatalf("WriteConfined must succeed under a raw symlinked-ancestor root: %v", err)
	}
	// Reads back byte-identical through the raw path...
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back through raw root: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-trip = %q, want %q", got, body)
	}
	// ...and it is the SAME real file when addressed via the de-symlinked real path.
	viaReal := filepath.Join(realRoot, ".nilcore", "artifacts", "id.json")
	got2, err := os.ReadFile(viaReal)
	if err != nil {
		t.Fatalf("read back via the real (de-symlinked) path: %v", err)
	}
	if string(got2) != string(body) {
		t.Errorf("de-symlinked read = %q, want %q — the write landed off the real path", got2, body)
	}
}

// TestWriteConfinedRefusesSwappedInWorktreeSymlinkRawRoot is the RAW-root variant of the
// parent-symlink-swap refusal: same swapped IN-WORKTREE symlink (BELOW root) pointing OUT
// of the tree, but with the root passed RAW through a symlinked host ancestor. It must
// still REFUSE — the fix must reject the in-worktree swap regardless of whether the root
// was passed raw or canonical.
func TestWriteConfinedRefusesSwappedInWorktreeSymlinkRawRoot(t *testing.T) {
	rawRoot, realRoot := symlinkedHostAncestorRoot(t)
	outside := resolveDir(t)
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// "sub" is an IN-WORKTREE (below-root) symlink to the outside dir — the swapped parent.
	link := filepath.Join(rawRoot, "sub")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	target := filepath.Join(rawRoot, "sub", "inner", "file.txt")
	if err := WriteConfined(rawRoot, target, []byte("ESCAPED"), 0o644); err == nil {
		t.Fatal("WriteConfined must refuse a swapped in-worktree symlink even under a raw root")
	}
	// Nothing may have been written through the swapped symlink — neither via the outside
	// dir nor via the real de-symlinked worktree path.
	if _, err := os.Stat(filepath.Join(outside, "inner", "file.txt")); err == nil {
		t.Fatal("confinement breached: a file was created outside through a swapped in-worktree symlink")
	}
	_ = realRoot
}

// TestWriteConfinedEvverifyEscapePattern is the exact escape being closed: the evverify
// write-back shape — RAW root + RAW target under it, BOTH containing a symlinked host
// ancestor (macOS `/var`) — combined with a swapped IN-WORKTREE symlink component. Before
// the fix, canonRoot canonicalized to /private/var/... while clean still started /var/...,
// so filepath.Rel returned a spurious ../../.. escape, the correct in-worktree symlink
// check was SKIPPED, and the fallback EvalSymlinks-resolved the whole existing prefix —
// FOLLOWING the attacker's in-worktree symlink OUT of the tree. The write must now be
// REFUSED and no byte may land outside the worktree.
func TestWriteConfinedEvverifyEscapePattern(t *testing.T) {
	rawRoot, realRoot := symlinkedHostAncestorRoot(t)
	outside := resolveDir(t)
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// A deeper in-worktree structure so the swapped symlink is a genuine below-root parent
	// component (mirrors `.nilcore/artifacts/<id>.json` write-backs): root/.nilcore is a
	// real dir, root/.nilcore/artifacts is swapped for a symlink pointing OUTSIDE.
	realNilcore := filepath.Join(rawRoot, ".nilcore")
	if err := os.Mkdir(realNilcore, 0o755); err != nil {
		t.Fatal(err)
	}
	swapped := filepath.Join(realNilcore, "artifacts")
	if err := os.Symlink(outside, swapped); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// The evverify target: absolute, under the RAW box.Workdir() root.
	target := filepath.Join(rawRoot, ".nilcore", "artifacts", "shard-1.json")
	if err := WriteConfined(rawRoot, target, []byte("ESCAPED VERDICT"), 0o644); err == nil {
		t.Fatal("WriteConfined must refuse the evverify escape (swapped in-worktree symlink under a raw root)")
	}
	// The verdict must NOT have landed in the attacker's outside directory.
	if _, err := os.Stat(filepath.Join(outside, "shard-1.json")); err == nil {
		t.Fatal("confinement breached: the evverify write escaped the worktree through a swapped symlink")
	}
	// And nothing landed via the real de-symlinked worktree path either (the symlink was
	// never a real dir, so this must simply not exist).
	if _, err := os.Stat(filepath.Join(realRoot, ".nilcore", "artifacts", "shard-1.json")); err == nil {
		t.Fatal("confinement breached: a file exists at the swapped-symlink target")
	}
}

// TestMkdirAllNoFollowCreatesRealDirs: a nested path of ordinary directories is
// created correctly (the happy path must not regress).
func TestMkdirAllNoFollowCreatesRealDirs(t *testing.T) {
	root := resolveDir(t)
	dir := filepath.Join(root, "a", "b", "c")
	if err := mkdirAllNoFollow(root, dir); err != nil {
		t.Fatalf("mkdirAllNoFollow on ordinary nested dirs: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("expected a real directory at %q: fi=%v err=%v", dir, fi, err)
	}
	// Idempotent: a second call over existing real dirs must succeed.
	if err := mkdirAllNoFollow(root, dir); err != nil {
		t.Fatalf("mkdirAllNoFollow must be idempotent over existing dirs: %v", err)
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

// TestWriteRefusesGitCaseVariants is the regression for the case-insensitive .git
// bypass: on macOS/Windows a write to ".GIT/config" resolves to the REAL .git/config,
// so the guard must fold case (and strip trailing dots/spaces for Windows). Without
// the fold, ".GIT/config" slips past hasGitComponent and plants a repo-local
// filter.*.clean / diff.external RCE that the host-side `git` tool then executes on the
// next `add`/`diff`. This test is meaningful on a case-insensitive FS (the maintainer's
// macOS default) where the bypass is live; on a case-sensitive FS the variants resolve
// to distinct harmless files and the guard's over-restriction is still asserted.
func TestWriteRefusesGitCaseVariants(t *testing.T) {
	root := resolveDir(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("[core]\n\trepositoryformatversion = 0\n")
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	payload := []byte("[filter \"x\"]\n\tclean = /pwned\n")
	for _, rel := range []string{
		".git/config", ".GIT/config", ".Git/config", ".gIt/config", // case fold (proven live on macOS)
		"sub/.GIT/hooks/pre-commit", // nested
		".git", ".GIT",              // the .git dir/pointer entry itself
		".git.", ".git ", ".GIT.", // Windows trailing-dot/space normalization
	} {
		if err := WriteAtomic(root, filepath.FromSlash(rel), payload, 0o644); err == nil {
			t.Errorf("WriteAtomic(%q) must be refused (resolves to .git)", rel)
		}
	}
	// The real .git/config must be byte-identical — no case/dot bypass landed in it.
	if got, _ := os.ReadFile(filepath.Join(root, ".git", "config")); string(got) != string(sentinel) {
		t.Fatalf(".git/config was modified through a case/dot bypass:\n%s", got)
	}
	// git-ADJACENT names (not ".git") must stay writable.
	for _, rel := range []string{".gitignore", ".gitattributes", ".github/workflows/ci.yml", "src/main.go"} {
		if err := WriteAtomic(root, filepath.FromSlash(rel), []byte("ok\n"), 0o644); err != nil {
			t.Errorf("WriteAtomic(%q) must be allowed (not .git): %v", rel, err)
		}
	}
}
