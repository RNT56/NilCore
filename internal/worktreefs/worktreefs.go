// Package worktreefs is the single audited copy of NilCore's worktree-confinement
// discipline: the symlink-safe path-join, the O_NOFOLLOW open, and the atomic
// temp-file + rename write that together guarantee a host-side file operation can
// never read or write outside one disposable worktree root (CLAUDE.md §2 I4).
//
// WHY a leaf. This logic lived unexported in internal/tools/fs.go. Three new
// consumers — the artifact store, the report writer, and the verifier write-back —
// each need the same guarantees. Re-implementing the most security-load-bearing
// code in the tree three times multiplies the audit surface and the blast radius of
// a single mistake. So it is extracted here as a stdlib-only LEAF with ZERO nilcore
// imports: internal/tools, internal/artifact, and internal/report all import this
// one copy. (Auditor B1.)
//
// The package performs scoped file I/O only — it never executes a program. It is the
// host-side, worktree-confined exception §2 I4 sanctions, nothing more.
package worktreefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// SafeJoin resolves rel against root and returns the cleaned absolute target,
// confirming the target stays inside root — both lexically AND after following
// symlinks. A lexical check alone is not enough: an in-tree symlink (e.g.
// `evil -> /etc`) would otherwise let access escape, so we resolve the deepest
// existing ancestor of the target through EvalSymlinks and re-check containment.
//
// Escape is rejected by confinement, not by pre-filtering the input: a `..` that
// climbs out of root, or a symlink that points out, is refused; a `..` that stays
// inside (e.g. "a/../b") and forward-only separators (".nilcore/artifacts/x.json")
// are allowed. An ABSOLUTE rel is joined onto root the way filepath.Join does
// (filepath.Join(root, "/x") == root/x), so it lands harmlessly INSIDE root rather
// than at the host absolute path — preserving the long-standing behavior of the
// extracted internal/tools primitive (a write addressed at an outside absolute path
// creates a shadow file under the worktree, never touching the real outside file).
// The returned target may not yet exist (a new file): callers pair SafeJoin with
// OpenNoFollow / WriteAtomic to close the final-component TOCTOU window.
func SafeJoin(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	resolvedRoot = filepath.Clean(resolvedRoot)
	target := filepath.Clean(filepath.Join(resolvedRoot, rel))
	return confine(resolvedRoot, target, rel)
}

// SafeAbs confirms an ABSOLUTE path resolves inside root (symlink-safe), returning
// the cleaned path. It is the read-root counterpart of SafeJoin: where SafeJoin
// joins a relative path onto a worktree, SafeAbs validates a caller-supplied
// absolute path against one allowed root. Used for READS against the worktree or an
// explicitly-added read root — never for a write.
func SafeAbs(root, abs string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	return confine(filepath.Clean(resolvedRoot), filepath.Clean(abs), abs)
}

// confine confirms target stays inside root — both lexically AND after following
// symlinks — returning target on success. ref is the original path, for the error.
func confine(root, target, ref string) (string, error) {
	if !within(root, target) {
		return "", fmt.Errorf("path %q escapes its root", ref)
	}
	// Resolve the deepest existing ancestor (the target itself may not exist yet,
	// e.g. a new file) and confirm it still resolves inside the root — this is what
	// catches an in-tree symlink pointing out.
	probe := target
	for {
		if _, lerr := os.Lstat(probe); lerr == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", ref, err)
	}
	if !within(root, filepath.Clean(real)) {
		return "", fmt.Errorf("path %q resolves outside its root (symlink escape)", ref)
	}
	return target, nil
}

// within reports whether p is root or lives under it.
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// OpenNoFollow opens path with O_NOFOLLOW added to flag, so a symlink at the final
// component is refused (ELOOP) rather than followed. It is the symlink-safe open
// primitive every write path uses; reads that must defend the final component use
// it too. The caller owns the returned *os.File.
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}

// ReadConfined reads the whole regular file at an already-confined absolute target
// (produced by SafeJoin / SafeAbs) WITHOUT following a symlink at the final
// component. It is the read counterpart of WriteConfined: confine() checks the path
// at CHECK time, but a concurrently-running sandboxed process can swap the final
// component for a symlink between that check and a plain os.ReadFile (a TOCTOU
// window), leaking an out-of-worktree file. Opening with O_NOFOLLOW closes that
// window — a swapped-in symlink is refused (ELOOP) rather than followed — so the
// bytes returned are always from the confined regular file, never a redirected one.
//
// A symlink at the final component fails closed with an error; a directory is
// rejected. The caller is responsible for confining target first (this mirrors
// WriteConfined's contract).
func ReadConfined(target string) ([]byte, error) {
	f, err := OpenNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("read %q: is a directory", target)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// WriteAtomic writes data to root/rel atomically and without following a symlink at
// the destination.
//
// Confinement: rel is resolved through SafeJoin first, so a destination that escapes
// root (lexically or via an in-tree symlink) is rejected before any byte is written.
//
// Atomicity: we never truncate-in-place. We write the full content into a freshly
// O_CREATE|O_EXCL|O_NOFOLLOW temp file in the SAME directory (so os.Rename stays on
// one filesystem and is atomic on POSIX), fsync it so the bytes are durable before
// the rename, then os.Rename it over the target. A kill of the harness at any point
// leaves either the old file untouched or the complete new file — never a
// half-applied, truncated file.
//
// Symlink safety: the temp file is opened with O_NOFOLLOW under a random,
// not-yet-existing name, so a symlink swapped in at the temp path cannot be followed
// or clobbered; and os.Rename does not write THROUGH a symlink at the destination —
// it replaces the name — so a swap at the destination after SafeJoin's check cannot
// redirect the write outside root.
//
// perm <= 0 means "preserve the destination's existing permissions on overwrite,
// else default 0644"; a positive perm is used verbatim.
func WriteAtomic(root, rel string, data []byte, perm os.FileMode) error {
	target, err := SafeJoin(root, rel)
	if err != nil {
		return err
	}
	// SafeJoin already resolved root through EvalSymlinks to produce target, so the
	// canonical boundary is that same resolved root. Re-resolve it here to pass a
	// canonical boundary to writeNoFollow's stepwise mkdir — the no-follow check is
	// bounded to components at/below this boundary, never the host's own ancestors.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	return writeNoFollow(filepath.Clean(resolvedRoot), target, data, perm)
}

// WriteConfined performs the same atomic, symlink-safe write as WriteAtomic but
// against a target path the CALLER has already confined (e.g. via SafeJoin or
// SafeAbs). It exists for call sites that already hold a validated absolute target
// and must not re-resolve it through SafeJoin (which would fail when the target's
// parent directory does not exist yet — the common "first write to a new nested
// path" case). The atomic temp+rename + O_NOFOLLOW discipline is identical; only the
// confinement step is the caller's responsibility.
//
// root is the worktree/confinement boundary target lives under. The stepwise
// no-follow mkdir rejects a symlinked directory component only at or below this
// boundary (an in-worktree parent a sandboxed process could swap — I4); components
// ABOVE the boundary are the host's own stable filesystem (e.g. a `/var`→`/private/var`
// symlink on macOS, a symlinked $TMPDIR/home) which are not attacker-writable in the
// sandbox threat model and are therefore trusted, not rejected. Pass the same
// worktree root whose join produced target; if a site truly has no root, the dir of
// target is a safe fallback (target's own parent is at/below the boundary).
//
// perm <= 0 ⇒ preserve existing perms on overwrite (default 0644 for a new file).
func WriteConfined(root, target string, data []byte, perm os.FileMode) error {
	return writeNoFollow(root, target, data, perm)
}

// mkdirAllNoFollow creates dir and any missing ancestors, refusing to traverse a
// symlinked component — but ONLY for components at or below the confinement root. It
// is the symlink-safe replacement for os.MkdirAll used on the write path: os.MkdirAll
// happily walks THROUGH an existing directory component even when that component is a
// symlink, so a parent-directory symlink swapped in after the caller's confinement
// check could redirect the write outside the worktree. Here, for every ancestor below
// the root that already exists we os.Lstat it (which does not follow the link) and
// reject it if it is a symlink; a missing component is created with os.Mkdir (which
// never follows a link at the component being created). A concurrent creator racing us
// to a component is tolerated (ErrExist), then re-validated as a real directory.
//
// Why the check is BOUNDED to root. The threat model (I4) is a sandboxed process
// swapping an IN-WORKTREE directory for a symlink to escape confinement — those
// components live at or below the worktree root and MUST still be rejected. The
// components ABOVE the root are the host's own stable filesystem: on macOS the system
// temp dir is under `/var/folders/...` where `/var` is a symlink to `/private/var`, and
// a user's $TMPDIR or home can likewise be symlinked. Those ancestors are not
// attacker-writable in the sandbox threat model, so rejecting them wrongly fails
// legitimate writes (the regression this fixes). We therefore Lstat/validate ONLY the
// components strictly below the canonical root and NEVER the root itself or anything
// above it — the root is resolved through EvalSymlinks first so it names a real dir.
//
// root and dir must both be absolute. dir is normally at or below the canonical root
// (callers pair SafeJoin/SafeAbs — which resolve root through EvalSymlinks — with this).
// When root is unusable as a boundary — it does not exist, cannot be resolved, or dir
// is not under it (e.g. a call site whose "root" is a placeholder while the real target
// lives elsewhere) — we fall back to bounding the check at dir's OWN longest existing
// prefix. That fallback is still safe: it trusts only ancestors that ALREADY EXIST as
// real directories and validates every component created below them, so an attacker
// cannot pre-plant a symlink under the not-yet-created tail.
func mkdirAllNoFollow(root, dir string) error {
	clean := filepath.Clean(dir)
	canonRoot, rel, err := boundaryTail(root, clean)
	if err != nil {
		return err
	}
	if rel == "." {
		// dir IS the boundary: it already exists as a real dir (EvalSymlinks succeeded).
		return nil
	}

	// Walk ONLY the components below canonRoot, creating/validating each. The root and
	// every ancestor above it are trusted and never Lstat'd or rejected here.
	sep := string(os.PathSeparator)
	prefix := canonRoot
	for _, comp := range strings.Split(rel, sep) {
		if comp == "" || comp == "." {
			continue
		}
		prefix = filepath.Join(prefix, comp)
		fi, lerr := os.Lstat(prefix)
		if lerr == nil {
			// Component exists: it must be a REAL directory. A symlink here is exactly
			// the swapped-in-parent escape we refuse to traverse (it is below the root).
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to traverse symlinked path component %q", prefix)
			}
			if !fi.IsDir() {
				return fmt.Errorf("path component %q is not a directory", prefix)
			}
			continue
		}
		if !errors.Is(lerr, fs.ErrNotExist) {
			return fmt.Errorf("stat path component %q: %w", prefix, lerr)
		}
		// Missing: create just this component. os.Mkdir does not follow a symlink at
		// the leaf being created. A racing creator (ErrExist) is fine as long as what
		// now exists is a real directory (re-validated just below).
		if err := os.Mkdir(prefix, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("mkdir %q: %w", prefix, err)
		}
		// Re-validate after a possible race: whatever exists now must be a real dir.
		fi, lerr = os.Lstat(prefix)
		if lerr != nil {
			return fmt.Errorf("stat created component %q: %w", prefix, lerr)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to traverse symlinked path component %q", prefix)
		}
		if !fi.IsDir() {
			return fmt.Errorf("path component %q is not a directory", prefix)
		}
	}
	return nil
}

// boundaryTail returns the canonical confinement boundary (canonRoot) and the
// slash-relative tail of clean below it. clean must be an absolute, cleaned path.
//
// THE ROOT-vs-clean SYMLINK-SPACE RULE. The no-follow walk that consumes this result
// must (a) TRUST a symlinked ancestor ABOVE the worktree root (the host's own stable
// filesystem — e.g. macOS `/var`→`/private/var`, a symlinked $TMPDIR/home — which is
// not attacker-writable in the sandbox threat model) while (b) REJECTING any swapped
// IN-WORKTREE symlink AT OR BELOW the root (the I4 escape). To decide (a) vs (b)
// correctly we must compare root and clean in the SAME symlink-space — otherwise a
// caller who passes a RAW root and a RAW target that both contain a symlinked host
// ancestor (exactly evverify's `Root = box.Workdir()` shape) produces a spurious
// escape at the Rel() step, and the OLD fallback then EvalSymlinks-resolved the whole
// existing prefix of clean — FOLLOWING an attacker-swapped in-worktree symlink out of
// the tree. That was the confirmed hole.
//
// The fix canonicalizes BOTH sides consistently:
//   - canonRoot   = EvalSymlinks(root), cleaned.
//   - canonClean  = clean's LONGEST-EXISTING PREFIX resolved through EvalSymlinks with
//     the not-yet-existing tail re-appended UNRESOLVED (resolveExistingPrefix).
//
// tail := Rel(canonRoot, canonClean). Two outcomes fall out correctly, fail-closed:
//
//	(a) A legit host-ancestor symlink above root is resolved on BOTH sides, so the two
//	    canonical paths AGREE on that ancestor — the tail is the real in-worktree
//	    suffix, and the caller walks it from canonRoot, Lstat-rejecting any symlink
//	    component and Mkdir-ing missing ones no-follow.
//	(b) A swapped IN-WORKTREE symlink pointing OUTSIDE lives in clean's existing prefix,
//	    so resolveExistingPrefix FOLLOWS it and canonClean lands OUTSIDE canonRoot →
//	    Rel escapes → we REJECT (error). The write never happens.
//
// If root cannot be resolved yet (it does not exist), we fall back to bounding at
// canonClean's own longest-existing prefix — every trusted ancestor there is an
// already-real directory, and every not-yet-created component below it is still created
// + re-validated no-follow by the caller.
func boundaryTail(root, clean string) (canonRoot, rel string, err error) {
	// Canonicalize clean in the SAME symlink-space we canonicalize root in: resolve its
	// longest existing prefix (following any in-worktree symlink there — which is the
	// point: a swapped one escapes and gets rejected below) and re-append the missing tail.
	canonClean, cerr := resolveExistingPrefix(clean)
	if cerr != nil {
		return "", "", fmt.Errorf("resolve target %q: %w", clean, cerr)
	}

	// Prefer root as the boundary — resolved through EvalSymlinks so a symlinked
	// ANCESTOR of root is trusted. Because canonClean is resolved in the same space, a
	// legit host-ancestor symlink agrees on both sides (tail is the real suffix) while a
	// swapped in-worktree symlink makes canonClean escape canonRoot.
	if canon, rerr := filepath.EvalSymlinks(root); rerr == nil {
		canon = filepath.Clean(canon)
		r, relErr := filepath.Rel(canon, canonClean)
		if relErr != nil || escapes(r) {
			// root resolved fine, but canonClean lands OUTSIDE it. In the same
			// symlink-space this can only mean an in-worktree symlink at/below root was
			// FOLLOWED out of the tree — the I4 escape. Fail closed; do NOT fall through
			// to a canonClean-rooted boundary (that would trust the attacker's target).
			return "", "", fmt.Errorf("target %q resolves outside its root (symlink escape)", clean)
		}
		return canon, r, nil
	}

	// Fallback boundary — reached ONLY when root itself is not resolvable (it does not
	// exist yet): bound at canonClean's OWN longest-existing prefix. That prefix names
	// only already-real directories, so trusting it is safe while every not-yet-created
	// component below it is still created + re-validated no-follow.
	boundary, berr := deepestExisting(canonClean)
	if berr != nil {
		return "", "", fmt.Errorf("resolve boundary of %q: %w", clean, berr)
	}
	r, relErr := filepath.Rel(boundary, canonClean)
	if relErr != nil || escapes(r) {
		return "", "", fmt.Errorf("target %q is not within a resolvable boundary", clean)
	}
	return boundary, r, nil
}

// deepestExisting returns p's longest ancestor (or p itself) that currently exists.
func deepestExisting(p string) (string, error) {
	probe := filepath.Clean(p)
	for {
		if _, lerr := os.Lstat(probe); lerr == nil {
			return probe, nil
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return probe, nil // reached the volume root
		}
		probe = parent
	}
}

// escapes reports whether a filepath.Rel result climbs out of its base (".." or a
// leading "../"), i.e. the path is NOT at or below the base.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// resolveExistingPrefix resolves p through EvalSymlinks by walking up to its longest
// existing ancestor (p itself may not exist yet — a new nested file/dir), then
// re-appending the missing tail. It mirrors confine()'s deepest-existing-ancestor
// probe: symlinked ancestors are canonicalized while the not-yet-created suffix is
// preserved, so the returned path is comparable against a canonical root.
func resolveExistingPrefix(p string) (string, error) {
	probe := filepath.Clean(p)
	var missing []string
	for {
		if _, lerr := os.Lstat(probe); lerr == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		missing = append([]string{filepath.Base(probe)}, missing...)
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	real = filepath.Clean(real)
	if len(missing) == 0 {
		return real, nil
	}
	return filepath.Join(append([]string{real}, missing...)...), nil
}

// hasGitComponent reports whether rel — an OS-separated path relative to the
// worktree root — has a component that a real filesystem would resolve to ".git"
// (the entry itself or any segment inside a ".git" directory). It matches the way a
// case-insensitive / name-normalizing filesystem resolves names: fold case (macOS
// APFS/HFS+ and Windows are case-insensitive, so ".GIT"/".Git" open the real ".git")
// and strip trailing dots and spaces (Windows strips them, so ".git." and ".git "
// also open ".git"). Without this, `.GIT/config` slips past the guard on macOS and
// lands in the REAL .git/config — reopening the repo-local-config RCE (a filter.*.clean
// planted there runs on the next host-side `git add`). On a case-sensitive FS this is a
// harmless over-restriction (a genuine ".GIT" directory is not a git dir).
// ".gitignore"/".gitattributes"/".github" differ from ".git" and stay writable.
func hasGitComponent(rel string) bool {
	for _, comp := range strings.Split(rel, string(os.PathSeparator)) {
		if strings.EqualFold(strings.TrimRight(comp, ". "), ".git") {
			return true
		}
	}
	return false
}

// writeNoFollow performs the atomic temp + rename against an already-confined target
// path. root is the confinement boundary the no-follow parent-dir check is bounded to
// (see mkdirAllNoFollow): a symlinked component AT OR BELOW root is refused, one ABOVE
// it (the host's own stable ancestors, e.g. macOS `/var`→`/private/var`) is trusted.
// perm <= 0 ⇒ preserve existing perms on overwrite (default 0644 for a new file); a
// positive perm is used verbatim.
func writeNoFollow(root, p string, content []byte, perm os.FileMode) error {
	// Refuse to write into a repository's .git metadata. This is the single chokepoint
	// every host-side writer funnels through (WriteAtomic and WriteConfined both land
	// here), so the guard covers the file tools, patch, format, edit_checked, rename,
	// structural-replace and plan at once. The host-side `git` tool runs REAL git in
	// this worktree, and repo-local .git/config (diff.external, filter.*.clean/smudge,
	// gpg.program) plus .git/hooks are host code-execution vectors — a model can drive
	// these writers with an attacker-chosen path, so planting .git metadata (or
	// repointing the .git gitdir file) through a file write must not be possible
	// (CLAUDE.md §2 I4). Git manages .git through its OWN process, never through this
	// path, so legitimate VCS operations are unaffected. boundaryTail yields p's tail
	// below the canonical worktree root — computed in the same symlink-space, so a
	// symlinked host ancestor cannot spoof the relative path; on the escape/unresolvable
	// error we skip (mkdirAllNoFollow rejects the same case just below, fail-closed).
	if _, rel, err := boundaryTail(root, filepath.Clean(p)); err == nil && hasGitComponent(rel) {
		return fmt.Errorf("refusing to write inside .git: %q", p)
	}

	dir := filepath.Dir(p)
	// Create parent dirs with a symlink-safe stepwise walk instead of os.MkdirAll:
	// MkdirAll follows an EXISTING directory component even if it is a symlink, so a
	// parent-directory symlink swapped in AFTER the caller's confinement check
	// (SafeJoin/SafeAbs) would let the eventual write land outside the worktree
	// (a TOCTOU escape). mkdirAllNoFollow refuses to traverse any in-worktree component
	// that is a symlink, closing that window — while trusting the host's own ancestors
	// above root so a legitimately symlinked temp/home dir does not fail the write.
	if err := mkdirAllNoFollow(root, dir); err != nil {
		return err
	}

	if perm <= 0 {
		// Preserve the destination's existing permissions on overwrite; default 0644
		// for a new file. Lstat (not Stat) so a symlink at p is not followed here.
		perm = os.FileMode(0o644)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			perm = fi.Mode().Perm()
		}
	}

	// O_EXCL guarantees a brand-new file under a unique name; O_NOFOLLOW refuses a
	// symlink swapped in at the temp path. os.CreateTemp can't set these flags, so
	// retry on the (vanishingly rare) name collision ourselves.
	var f *os.File
	var tmp string
	for i := 0; ; i++ {
		tmp = filepath.Join(dir, fmt.Sprintf(".nilcore-tmp-%d-%d", os.Getpid(), randUint()))
		var err error
		f, err = OpenNoFollow(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
		if err == nil {
			break
		}
		if errors.Is(err, fs.ErrExist) && i < 1000 {
			continue
		}
		return fmt.Errorf("create temp file: %w", err)
	}

	// From here on, ensure the temp file never lingers if anything fails.
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// randUint returns a random uint64 for temp-file naming. It is only used to avoid
// name collisions, not for any security property — O_EXCL is what actually
// guarantees a fresh file — so the stdlib math/rand/v2 source is fine.
func randUint() uint64 { return rand.Uint64() }
