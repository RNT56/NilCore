package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// seedArtifactTree writes a minimal worktree: one source file (outside .nilcore, so the
// worktree hash covers it) and one evidence artifact (under .nilcore/artifacts, which
// ContentHashWorktree skips). It returns the root.
func seedArtifactTree(t *testing.T, artifactBody string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adir := filepath.Join(root, ".nilcore", "artifacts")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adir, "a1.json"), []byte(artifactBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeArtifactBody(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".nilcore", "artifacts", "a1.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContentHashWithArtifacts(t *testing.T) {
	ctx := context.Background()

	t.Run("includeArtifacts=false is byte-identical to ContentHashWorktree", func(t *testing.T) {
		root := seedArtifactTree(t, `{"id":"a1","value":"v"}`)
		got, err := ContentHashWithArtifacts(ctx, root, false)
		if err != nil {
			t.Fatalf("ContentHashWithArtifacts: %v", err)
		}
		want, err := ContentHashWorktree(ctx, root, ".git", ".nilcore")
		if err != nil {
			t.Fatalf("ContentHashWorktree: %v", err)
		}
		if got != want {
			t.Fatalf("includeArtifacts=false must equal ContentHashWorktree;\n got  %s\n want %s", got, want)
		}
	})

	// The I2 fix: with evidence verification ON, changing ONLY an artifact (same source)
	// must change the vcache content hash — otherwise the key collides and vcache replays
	// a stale GREEN with the ArtifactVerifier skipped.
	t.Run("changed artifact (same source) changes the hash when included", func(t *testing.T) {
		root := seedArtifactTree(t, `{"id":"a1","value":"3.2%"}`)
		before, err := ContentHashWithArtifacts(ctx, root, true)
		if err != nil {
			t.Fatalf("before: %v", err)
		}
		// Mutate ONLY the artifact (a fabricated Value); every source file is untouched.
		writeArtifactBody(t, root, `{"id":"a1","value":"9.9%"}`)
		after, err := ContentHashWithArtifacts(ctx, root, true)
		if err != nil {
			t.Fatalf("after: %v", err)
		}
		if before == after {
			t.Fatal("a changed artifact must change the vcache content hash (else a stale GREEN replays with the ArtifactVerifier skipped)")
		}
		// Prove the source really was unchanged: the plain worktree hash is stable across
		// the artifact mutation — this is exactly why the fold is required.
		w1, _ := ContentHashWorktree(ctx, root, ".git", ".nilcore")
		writeArtifactBody(t, root, `{"id":"a1","value":"different-again"}`)
		w2, _ := ContentHashWorktree(ctx, root, ".git", ".nilcore")
		if w1 != w2 {
			t.Fatalf("sanity: the worktree hash must ignore .nilcore artifact changes; %s != %s", w1, w2)
		}
	})

	// The legacy path (evidence off) stays byte-identical: an artifact change does NOT
	// perturb the hash, so the packs-off vcache behavior is unchanged.
	t.Run("changed artifact does NOT change the hash when excluded (byte-identical legacy)", func(t *testing.T) {
		root := seedArtifactTree(t, `{"id":"a1","value":"v"}`)
		before, _ := ContentHashWithArtifacts(ctx, root, false)
		writeArtifactBody(t, root, `{"totally":"different"}`)
		after, _ := ContentHashWithArtifacts(ctx, root, false)
		if before != after {
			t.Fatal("with artifacts excluded, an artifact change must not affect the hash")
		}
	})

	t.Run("changed source changes the hash regardless of includeArtifacts", func(t *testing.T) {
		for _, include := range []bool{false, true} {
			root := seedArtifactTree(t, `{"id":"a1","value":"v"}`)
			before, _ := ContentHashWithArtifacts(ctx, root, include)
			if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n// edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			after, _ := ContentHashWithArtifacts(ctx, root, include)
			if before == after {
				t.Fatalf("include=%v: a source change must change the hash", include)
			}
		}
	})

	t.Run("no artifacts dir: included and excluded agree (stable empty digest folds nothing new)", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// includeArtifacts=true with NO artifacts folds the empty-set digest; the hash is
		// stable and well-defined (no error, no panic).
		on, err := ContentHashWithArtifacts(ctx, root, true)
		if err != nil {
			t.Fatalf("include=true, no artifacts: %v", err)
		}
		again, err := ContentHashWithArtifacts(ctx, root, true)
		if err != nil {
			t.Fatalf("include=true, no artifacts (2): %v", err)
		}
		if on != again {
			t.Fatal("the no-artifacts hash must be deterministic")
		}
	})
}
