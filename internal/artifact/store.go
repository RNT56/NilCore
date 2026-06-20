package artifact

// store.go is the worktree persistence layer for artifacts (Phase 11, P11-T02).
// Artifacts live as JSON at the FIXED out-of-band path .nilcore/artifacts/<id>.json
// inside one disposable worktree root — the carrier every pillar (evverify write-back,
// typed worker results, requeue, report) agrees on. They ride entirely beside
// backend.Task/Result (I1) and never reach the network: this file does scoped file
// I/O only.
//
// All path safety is delegated to internal/worktreefs — the single audited copy of
// the symlink-safe join + O_NOFOLLOW + atomic-temp-rename discipline (I4). This
// package hand-rolls NO EvalSymlinks/OpenFile of its own; it only validates that the
// artifact id is a single path component so the fixed <id>.json layout cannot be
// subverted into a nested or escaping path.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"nilcore/internal/worktreefs"
)

// artifactDir is the fixed, out-of-band directory (relative to the worktree root)
// where every artifact JSON file lives. It is a constant the whole spine agrees on.
const artifactDir = ".nilcore/artifacts"

// ErrNotFound is returned by Read when no artifact file exists for the id. It is
// distinct (errors.Is-distinguishable) from a parse error so a caller can tell
// "nothing written yet" apart from "a corrupt file is on disk" — the requeue/report
// pillars route those two cases differently.
var ErrNotFound = errors.New("artifact: not found")

// validID rejects any id that is not a single, safe path component, so the fixed
// .nilcore/artifacts/<id>.json layout can never be bent into a nested or escaping
// path BEFORE worktreefs is even consulted. We reject the empty id, "."/"..", any
// path separator (both '/' and the OS separator), and a leading dot (which would
// produce a hidden/temp-looking file). worktreefs.SafeJoin is still the authoritative
// confinement check downstream — this is a narrowing precondition, not a substitute.
func validID(id string) error {
	if id == "" {
		return fmt.Errorf("artifact: empty id")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("artifact: invalid id %q", id)
	}
	if strings.ContainsRune(id, '/') || strings.ContainsRune(id, os.PathSeparator) {
		return fmt.Errorf("artifact: id %q must be a single path component (no separators)", id)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("artifact: id %q must not start with a dot", id)
	}
	return nil
}

// relPath is the worktree-relative path for an artifact id. It uses path.Join (not
// filepath.Join) so the relative slug is always forward-slashed; worktreefs converts
// to the host separator when it joins onto the absolute root.
func relPath(id string) string {
	return path.Join(artifactDir, id+".json")
}

// Write serializes a to canonical JSON and persists it at
// <root>/.nilcore/artifacts/<a.ID>.json via worktreefs.WriteAtomic — atomic
// (temp + rename), symlink-safe (O_NOFOLLOW), and confined to root. An id that is
// not a single safe path component is rejected before any byte is written, so no
// escape file can be created. The 0 perm lets worktreefs default/preserve the mode.
func Write(root string, a *Artifact) error {
	if a == nil {
		return fmt.Errorf("artifact: write nil artifact")
	}
	if err := validID(a.ID); err != nil {
		return err
	}
	data, err := Marshal(a)
	if err != nil {
		return err
	}
	if err := worktreefs.WriteAtomic(root, relPath(a.ID), data, 0); err != nil {
		return fmt.Errorf("artifact: write %q: %w", a.ID, err)
	}
	return nil
}

// Read loads the artifact persisted for id from <root>/.nilcore/artifacts/<id>.json.
// It resolves the path through worktreefs.SafeJoin (symlink-safe confinement) and
// opens it with worktreefs.OpenNoFollow, so a symlink planted at the target path is
// refused (fail-closed) rather than followed. A missing file returns ErrNotFound
// (errors.Is-distinguishable); corrupt JSON returns a wrapped parse error — never a
// silent zero-value.
func Read(root, id string) (*Artifact, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	target, err := worktreefs.SafeJoin(root, relPath(id))
	if err != nil {
		return nil, fmt.Errorf("artifact: read %q: %w", id, err)
	}
	f, err := worktreefs.OpenNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("artifact: read %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("artifact: read %q: %w", id, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("artifact: read %q: %w", id, err)
	}
	a, err := Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("artifact: read %q: %w", id, err)
	}
	return a, nil
}
