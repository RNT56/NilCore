package vcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nilcore/internal/worktreefs"
)

// HashWorktree returns a Hasher that computes a deterministic content hash over the
// entire worktree rooted at root. It is the CONSERVATIVE default the package
// promises: the hash must cover everything the check reads, and absent a precise
// read-set the safe over-approximation is "the whole tree". A change anywhere in
// the worktree therefore changes the key and forces a recompute — the cache can
// only ever skip a run when the tree is byte-for-byte what it was at the last pass.
//
// Determinism. Entries are walked, sorted by relative path, and folded in a fixed
// order: for each file, its path, mode bits, and a SHA-256 of its bytes; for each
// directory and symlink, its path and a type tag (a symlink also folds its target,
// since flipping where a link points changes what the build sees). The result is
// stable across runs and platforms for the same tree, so the same content always
// yields the same key.
//
// Confinement. Every path is validated through worktreefs.SafeAbs before it is
// opened, and files are read with O_NOFOLLOW, so the hash walk obeys the same
// worktree-confinement discipline as the rest of the host-side file tools (§2 I4):
// an in-tree symlink can never make the hasher read outside root.
//
// Skips. The version-control metadata directory (.git) and the worktree's own
// scratch/log directory (.nilcore) are excluded: they are not part of the source
// the verifier checks, and folding the event log into the key would make the cache
// self-invalidating (every recorded pass would change the very hash that keys it).
func HashWorktree(root string, skipDirs ...string) Hasher {
	skip := map[string]bool{".git": true, ".nilcore": true}
	for _, d := range skipDirs {
		skip[d] = true
	}
	return func(ctx context.Context) (string, error) {
		return hashTree(ctx, root, skip)
	}
}

// fileDigest is one folded entry, kept so the final fold is order-independent of
// the walk and instead sorted by Rel for full determinism.
type fileDigest struct {
	Rel  string
	Line string // the canonical per-entry line folded into the tree hash
}

func hashTree(ctx context.Context, root string, skip map[string]bool) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}

	var entries []fileDigest
	walkErr := filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, rerr := filepath.Rel(resolvedRoot, path)
		if rerr != nil {
			return fmt.Errorf("relativize %q: %w", path, rerr)
		}
		if rel == "." {
			return nil
		}
		// Skip excluded top-level dirs (and everything under them).
		top := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if skip[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Confine before any open: the walked path must resolve inside root.
		if _, cerr := worktreefs.SafeAbs(resolvedRoot, path); cerr != nil {
			return fmt.Errorf("confine %q: %w", rel, cerr)
		}
		line, lerr := entryLine(rel, path, d)
		if lerr != nil {
			return lerr
		}
		if line != "" {
			entries = append(entries, fileDigest{Rel: rel, Line: line})
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("hashing worktree: %w", walkErr)
	}

	// Canonical order: sort by relative path so the tree hash is independent of the
	// OS-dependent walk order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })

	tree := sha256.New()
	for _, e := range entries {
		// Length-prefix each line so two different entry sets cannot fold to the same
		// digest by concatenation aliasing.
		fmt.Fprintf(tree, "%d:", len(e.Line))
		_, _ = io.WriteString(tree, e.Line)
		_, _ = tree.Write([]byte{0})
	}
	return hex.EncodeToString(tree.Sum(nil)), nil
}

// entryLine builds the canonical, deterministic line for one walked entry. A
// directory and a symlink fold structural identity only (a symlink also folds its
// target); a regular file folds its mode and a content digest. Anything else (a
// device node, socket, fifo) folds its type tag so its mere presence still affects
// the hash without us trying to read it.
func entryLine(rel, path string, d fs.DirEntry) (string, error) {
	switch {
	case d.IsDir():
		return "dir\x00" + rel, nil
	case d.Type()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("readlink %q: %w", rel, err)
		}
		return "link\x00" + rel + "\x00" + target, nil
	case d.Type().IsRegular():
		digest, mode, err := fileContentDigest(path)
		if err != nil {
			return "", fmt.Errorf("digest %q: %w", rel, err)
		}
		return fmt.Sprintf("file\x00%s\x00%o\x00%s", rel, mode, digest), nil
	default:
		return "other\x00" + rel + "\x00" + d.Type().String(), nil
	}
}

// fileContentDigest returns the hex SHA-256 of a regular file's bytes plus its
// permission bits, reading it with O_NOFOLLOW so a symlink swapped in at the final
// component is refused rather than followed.
func fileContentDigest(path string) (digest string, mode fs.FileMode, err error) {
	f, err := worktreefs.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), fi.Mode().Perm(), nil
}
