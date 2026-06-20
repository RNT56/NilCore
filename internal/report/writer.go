package report

// writer.go is the report worktree-writer (Phase 11, Pillar 6, P11-T31). It
// persists a rendered report (text/HTML/markdown — produced by P11-T32) to the
// FIXED out-of-band path .nilcore/reports/<run>.<ext> inside one disposable
// worktree root. Like the artifact store (P11-T02), it owns NO path safety of its
// own: every byte goes through internal/worktreefs — the single audited copy of
// the symlink-safe join + O_NOFOLLOW + atomic-temp-rename discipline (I4). This
// file does scoped file I/O only and never reaches the network.
//
// It is content-agnostic ON PURPOSE: it takes already-rendered bytes and a fixed
// extension, so the renderers stay pure functions and the writer stays the one
// confined sink. The two narrowing preconditions it enforces before worktreefs is
// even consulted — run must be a single safe path component, ext must be one of a
// closed allowlist — keep the fixed <run>.<ext> layout from being bent into a
// nested or escaping path.

import (
	"fmt"
	"os"
	"path"
	"strings"

	"nilcore/internal/worktreefs"
)

// reportsDir is the fixed, out-of-band directory (relative to the worktree root)
// where every rendered report file lives. It mirrors artifactDir's role for the
// artifact store: a constant the whole Pillar-6 surface agrees on.
const reportsDir = ".nilcore/reports"

// allowedExts is the closed set of report file extensions. It is deliberately a
// tiny allowlist (one per renderer in P11-T32) so a caller can never smuggle an
// arbitrary suffix — e.g. an executable or a dotfile-looking name — through the
// fixed <run>.<ext> layout.
var allowedExts = map[string]struct{}{
	"html": {},
	"md":   {},
	"txt":  {},
	"json": {}, // SW-T06: the redacted-projection JSON deliverable (render.MarshalRedacted)
}

// validRun rejects any run identifier that is not a single, safe path component,
// so the fixed .nilcore/reports/<run>.<ext> layout can never be bent into a nested
// or escaping path BEFORE worktreefs is consulted. It mirrors the artifact store's
// validID: reject the empty run, "."/"..", any path separator (both '/' and the OS
// separator), and a leading dot (which would produce a hidden/temp-looking file).
// worktreefs.SafeJoin remains the authoritative confinement check downstream — this
// is a narrowing precondition, not a substitute.
func validRun(run string) error {
	if run == "" {
		return fmt.Errorf("report: empty run")
	}
	if run == "." || run == ".." {
		return fmt.Errorf("report: invalid run %q", run)
	}
	if strings.ContainsRune(run, '/') || strings.ContainsRune(run, os.PathSeparator) {
		return fmt.Errorf("report: run %q must be a single path component (no separators)", run)
	}
	if strings.HasPrefix(run, ".") {
		return fmt.Errorf("report: run %q must not start with a dot", run)
	}
	return nil
}

// validExt confirms ext is one of the closed allowlist {html,md,txt}. The ext must
// also be a bare suffix (no dot, no separator) so it cannot itself escape the
// layout — the allowlist already guarantees this, but we keep the rejection
// explicit and actionable.
func validExt(ext string) error {
	if _, ok := allowedExts[ext]; !ok {
		return fmt.Errorf("report: ext %q not allowed (want one of html, md, txt)", ext)
	}
	return nil
}

// reportRelPath is the worktree-relative path for a rendered report. It uses
// path.Join (not filepath.Join) so the relative slug is always forward-slashed;
// worktreefs converts to the host separator when it joins onto the absolute root.
func reportRelPath(run, ext string) string {
	return path.Join(reportsDir, run+"."+ext)
}

// WriteReport persists content to <root>/.nilcore/reports/<run>.<ext> via
// worktreefs.WriteAtomic — atomic (temp + rename), symlink-safe (O_NOFOLLOW), and
// confined to root. It is content-agnostic: content is the already-rendered report
// bytes from P11-T32, written verbatim.
//
// run is validated to a single safe path component and ext to the closed allowlist
// {html,md,txt} BEFORE any byte is written, so no nested or escaping file can be
// created. A symlink planted at the target path is refused by worktreefs
// (fail-closed) rather than followed. The 0 perm lets worktreefs default/preserve
// the mode.
func WriteReport(root, run, ext string, content []byte) error {
	if err := validRun(run); err != nil {
		return err
	}
	if err := validExt(ext); err != nil {
		return err
	}
	if err := worktreefs.WriteAtomic(root, reportRelPath(run, ext), content, 0); err != nil {
		return fmt.Errorf("report: write %q.%s: %w", run, ext, err)
	}
	return nil
}
