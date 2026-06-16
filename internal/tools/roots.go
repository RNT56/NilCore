package tools

// roots.go adds READ-ONLY extra context roots to the read/search tools — the
// host-side half of "let the user add a repo / folder / files as context"
// (X-T01). It is the I4-critical boundary: a tool can read the worktree OR any
// explicitly-added read root, and NOTHING else.
//
// Invariant discipline (I4):
//   - WriteTool/EditTool are untouched — they resolve ONLY against the worktree, so
//     the single-writable-root guarantee holds. Extra roots are never writable.
//   - A RELATIVE path still resolves against the worktree exactly as before, so a
//     bare "read foo.go" can never reach an extra root (and "../escape" is still
//     rejected). Extra roots are addressed by ABSOLUTE path, and an absolute path is
//     allowed ONLY if it resolves (symlink-safe) inside the worktree or one added
//     root — "/etc/passwd" with no matching root is rejected.
//   - Each added root is resolved through EvalSymlinks when registered (see the
//     wiring site), and confine() re-checks symlink escape on every access.

import (
	"fmt"
	"path/filepath"
)

// resolveReadable resolves rel for a READ against the worktree or, when rel is
// absolute, against the worktree or any added read root. A relative path is
// worktree-only (today's behavior, byte-identical when readRoots is empty); an
// absolute path must land inside one allowed root or it is rejected.
func resolveReadable(workdir string, readRoots []string, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(rel) {
		return safePath(workdir, rel)
	}
	// Absolute: allow only if it resolves within the worktree or an added read root.
	if p, err := safeAbs(workdir, rel); err == nil {
		return p, nil
	}
	for _, root := range readRoots {
		if p, err := safeAbs(root, rel); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the worktree and any added context root", rel)
}
