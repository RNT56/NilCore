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
	return writeNoFollow(target, data, perm)
}

// WriteConfined performs the same atomic, symlink-safe write as WriteAtomic but
// against a target path the CALLER has already confined (e.g. via SafeJoin or
// SafeAbs). It exists for call sites that already hold a validated absolute target
// and must not re-resolve it through SafeJoin (which would fail when the target's
// parent directory does not exist yet — the common "first write to a new nested
// path" case). The atomic temp+rename + O_NOFOLLOW discipline is identical; only the
// confinement step is the caller's responsibility.
//
// perm <= 0 ⇒ preserve existing perms on overwrite (default 0644 for a new file).
func WriteConfined(target string, data []byte, perm os.FileMode) error {
	return writeNoFollow(target, data, perm)
}

// writeNoFollow performs the atomic temp + rename against an already-confined target
// path. perm <= 0 ⇒ preserve existing perms on overwrite (default 0644 for a new
// file); a positive perm is used verbatim.
func writeNoFollow(p string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
