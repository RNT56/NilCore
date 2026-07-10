package verify

// artifacts_hash.go — the vcache content-hash input for the EVIDENCE-VERIFY path.
//
// ContentHashWorktree deliberately skips the whole .nilcore/ scratch tree (enrich.go),
// which is correct for the general source-content hash. But when evidence verification
// is enabled the composed verifier reads its claim inputs from .nilcore/artifacts/*.json
// — files the source hash never covers. So a run that changes ONLY an artifact (same
// source) keeps the identical worktree hash, and the vcache would replay a stale GREEN
// with the ArtifactVerifier skipped (an I2 hole).
//
// ContentHashWithArtifacts closes that hole WITHOUT changing ContentHashWorktree's
// general skip (other callers rely on it): it returns the plain worktree hash and, only
// when includeArtifacts is set, folds in a digest over the sorted artifact files. With
// includeArtifacts=false it is byte-identical to ContentHashWorktree(root, ".git",
// ".nilcore") — the unchanged default path.
//
// I4: artifact files are read with O_NOFOLLOW and confined through worktreefs, exactly
// as the worktree walk does, so an in-tree symlink cannot redirect the read. I7: file
// CONTENT is hashed as opaque bytes only — never interpreted as instructions.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nilcore/internal/worktreefs"
)

// artifactsRelDir is the worktree-relative directory the evidence verifier reads its
// claim inputs from. Kept in sync with the cmd-layer artifactFiles discovery.
const artifactsRelDir = ".nilcore/artifacts"

// ContentHashWithArtifacts returns the deterministic vcache content hash for a worktree.
// It is ContentHashWorktree(root, ".git", ".nilcore) — and, when includeArtifacts is
// true, additionally folds in a digest over the .nilcore/artifacts/*.json files that the
// worktree hash skips. That extra fold is what makes a changed evidence artifact (same
// source) MISS the vcache, so the ArtifactVerifier re-runs instead of a stale GREEN being
// replayed (I2).
//
// includeArtifacts MUST mirror the condition that composes the ArtifactVerifier
// (NILCORE_EVIDENCE_VERIFY enabled). With includeArtifacts=false the result is
// byte-identical to the plain worktree hash — the evidence-off path is unchanged.
func ContentHashWithArtifacts(ctx context.Context, root string, includeArtifacts bool) (string, error) {
	base, err := ContentHashWorktree(ctx, root, ".git", ".nilcore")
	if err != nil {
		return "", err
	}
	if !includeArtifacts {
		return base, nil
	}
	art, err := contentHashArtifacts(ctx, root)
	if err != nil {
		return "", err
	}
	// Fold the worktree hash and the artifacts digest into one key, length-prefixed so
	// the two fields cannot alias by concatenation (mirrors foldEntries' discipline).
	h := sha256.New()
	fmt.Fprintf(h, "worktree:%d:", len(base))
	_, _ = h.Write([]byte(base))
	_, _ = h.Write([]byte{0})
	fmt.Fprintf(h, "artifacts:%d:", len(art))
	_, _ = h.Write([]byte(art))
	_, _ = h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil)), nil
}

// contentHashArtifacts returns a deterministic digest over the sorted
// .nilcore/artifacts/*.json files under root. A missing/empty directory yields the
// stable empty-set digest (foldEntries(nil)), so "evidence-verify on, no artifacts yet"
// hashes consistently rather than erroring. Each file's content is folded with the same
// canonical, type-tagged, length-prefixed line the worktree walk uses (entryLine's file
// shape), so a changed artifact byte changes the digest.
func contentHashArtifacts(ctx context.Context, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return foldEntries(nil), nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// No resolvable root ⇒ nothing to fold (the worktree hash already errored if the
		// root were truly unusable; for artifacts we degrade to the empty digest).
		return foldEntries(nil), nil
	}
	dir := filepath.Join(resolvedRoot, ".nilcore", "artifacts")
	dirents, err := os.ReadDir(dir)
	if err != nil {
		// No artifacts directory ⇒ empty, stable digest (no artifacts to fold).
		return foldEntries(nil), nil
	}

	var entries []hashEntry
	for _, de := range dirents {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		path := filepath.Join(dir, name)
		if _, serr := worktreefs.SafeAbs(resolvedRoot, path); serr != nil {
			return "", fmt.Errorf("confine artifact %q: %w", name, serr)
		}
		digest, mode, derr := fileContentDigest(path)
		if derr != nil {
			return "", fmt.Errorf("digest artifact %q: %w", name, derr)
		}
		rel := artifactsRelDir + "/" + name
		entries = append(entries, hashEntry{rel: rel, line: fmt.Sprintf("file\x00%s\x00%o\x00%s", rel, mode, digest)})
	}
	return foldEntries(entries), nil
}
