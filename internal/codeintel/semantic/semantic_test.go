package semantic

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// letterEmbedder is a fake deterministic Embedder: it maps text to the counts of
// a few fixed letters, so similar text yields similar vectors without any model.
type letterEmbedder struct{}

func (letterEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	var vec [4]float32 // counts of a, b, c, d
	for _, r := range text {
		switch r {
		case 'a', 'A':
			vec[0]++
		case 'b', 'B':
			vec[1]++
		case 'c', 'C':
			vec[2]++
		case 'd', 'D':
			vec[3]++
		}
	}
	return vec[:], nil
}

func openIndex(t *testing.T, e Embedder) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "sem.db"), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestSearchVectorRanksMostSimilarFirst(t *testing.T) {
	ctx := context.Background()
	ix := openIndex(t, letterEmbedder{})

	// "abcd" is the query; "aabbccdd" is its colinear (most similar) match,
	// while "dddd" is nearly orthogonal.
	docs := map[string]string{
		"balanced":  "aabbccdd",
		"d_heavy":   "dddd",
		"a_heavy":   "aaaa",
		"unrelated": "xyzxyz",
	}
	for id, text := range docs {
		if err := ix.Add(ctx, id, text); err != nil {
			t.Fatalf("Add(%q): %v", id, err)
		}
	}

	hits, err := ix.Search(ctx, "abcd", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("Search returned %d hits, want 3", len(hits))
	}
	if hits[0].ID != "balanced" {
		t.Errorf("top hit = %q (score %v), want balanced; full: %+v", hits[0].ID, hits[0].Score, hits)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Errorf("hits not score-descending: %+v", hits)
		}
	}
}

func TestSearchVectorTieBreakByID(t *testing.T) {
	ctx := context.Background()
	ix := openIndex(t, letterEmbedder{})

	// Identical text -> identical vectors -> identical scores; the tie must
	// break deterministically by id ascending.
	for _, id := range []string{"zeta", "alpha", "mu"} {
		if err := ix.Add(ctx, id, "abab"); err != nil {
			t.Fatalf("Add(%q): %v", id, err)
		}
	}
	hits, err := ix.Search(ctx, "abab", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []string{"alpha", "mu", "zeta"}
	for i, h := range hits {
		if h.ID != want[i] {
			t.Fatalf("tie-break order = %+v, want %v", hits, want)
		}
	}
}

func TestSearchLexicalFallback(t *testing.T) {
	ctx := context.Background()
	ix := openIndex(t, nil) // nil Embedder -> degrade to lexical

	docs := map[string]string{
		"parser":    "func parseConfig reads the YAML config file",
		"http":      "func serveHTTP handles incoming web requests",
		"sandbox":   "run untrusted commands inside the container sandbox",
		"unrelated": "completely different subject matter",
	}
	for id, text := range docs {
		if err := ix.Add(ctx, id, text); err != nil {
			t.Fatalf("Add(%q): %v", id, err)
		}
	}

	hits, err := ix.Search(ctx, "CONFIG", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search(CONFIG) = %+v, want exactly 1 hit", hits)
	}
	if hits[0].ID != "parser" {
		t.Errorf("lexical hit = %q, want parser", hits[0].ID)
	}
}

func TestSearchLexicalRanksByOverlap(t *testing.T) {
	ctx := context.Background()
	ix := openIndex(t, nil)

	if err := ix.Add(ctx, "both", "sandbox container executor"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Add(ctx, "one", "sandbox isolation only"); err != nil {
		t.Fatal(err)
	}

	hits, err := ix.Search(ctx, "sandbox container", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %+v, want 2 hits", hits)
	}
	if hits[0].ID != "both" {
		t.Errorf("top lexical hit = %q, want both (higher term overlap)", hits[0].ID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("expected both (%v) to outrank one (%v)", hits[0].Score, hits[1].Score)
	}
}

// TestReconcilePrunesStaleIDs proves the IDs+Delete reconciliation primitives let
// a persistent index shed a stored id absent from the live set — the fix for a
// renamed/deleted symbol whose INSERT OR REPLACE-only row would otherwise linger
// forever and resurface as a phantom hit. It also confirms Delete invalidates the
// vector graph so a pruned id no longer appears in search results.
func TestReconcilePrunesStaleIDs(t *testing.T) {
	ctx := context.Background()
	ix := openIndex(t, letterEmbedder{})

	// "OldName" is stale (renamed away); "Live" remains in the current tree.
	if err := ix.Add(ctx, "OldName", "abcd"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Add(ctx, "Live", "abcd"); err != nil {
		t.Fatal(err)
	}

	// Reconcile against the live set: prune any stored id not currently present.
	live := map[string]bool{"Live": true}
	ids, err := ix.IDs(ctx)
	if err != nil {
		t.Fatalf("IDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("IDs before prune = %v, want OldName+Live", ids)
	}
	for _, id := range ids {
		if !live[id] {
			if err := ix.Delete(ctx, id); err != nil {
				t.Fatalf("Delete(%q): %v", id, err)
			}
		}
	}

	// OldName is gone from the stored set...
	after, err := ix.IDs(ctx)
	if err != nil {
		t.Fatalf("IDs: %v", err)
	}
	if len(after) != 1 || after[0] != "Live" {
		t.Fatalf("IDs after prune = %v, want [Live]", after)
	}
	// ...and no longer surfaces in a search that its vector would have matched.
	hits, err := ix.Search(ctx, "abcd", 5)
	for _, h := range hits {
		if h.ID == "OldName" {
			t.Fatalf("pruned id OldName still returned by Search: %+v", hits)
		}
	}
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
}

func TestSearchEmptyK(t *testing.T) {
	ctx := context.Background()
	ix := openIndex(t, nil)
	if err := ix.Add(ctx, "x", "anything"); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search(ctx, "anything", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if hits != nil {
		t.Errorf("Search with k=0 = %+v, want nil", hits)
	}
}

// TestOpenMigratesLegacyDocsHashColumn covers the pragma-guarded hash-column migration
// (OpenModel): a docs table created before the content-hash column existed must gain it
// on Open — detected via pragma_table_info, NOT a driver-specific "duplicate column
// name" error string — and a re-Open must be an idempotent no-op.
func TestOpenMigratesLegacyDocsHashColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy_sem.db")

	// Pre-D2-T01 docs table: no hash column.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE docs (id TEXT PRIMARY KEY, text TEXT NOT NULL, vec TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO docs (id, text) VALUES ('a', 'legacy row')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Open must migrate the hash column in (guarded by pragma_table_info).
	ix, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open legacy semantic DB: %v", err)
	}
	if has, err := docsHasColumn(ix.db, "hash"); err != nil || !has {
		t.Fatalf("Open did not migrate docs.hash onto a legacy DB (has=%v err=%v)", has, err)
	}
	// The migrated column is usable: an Add records a content hash without error.
	if err := ix.Add(ctx, "b", "new row"); err != nil {
		t.Fatalf("Add after migration: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-Open is an idempotent no-op (pragma sees the column, skips the ALTER).
	ix2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("re-Open after hash migration (idempotency): %v", err)
	}
	t.Cleanup(func() { ix2.Close() })
}
