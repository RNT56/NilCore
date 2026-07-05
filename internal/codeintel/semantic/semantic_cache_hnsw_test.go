package semantic

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
)

// countingEmbedder wraps a deterministic embedding and tallies how many times
// Embed is called. The cache test asserts on that tally: an unchanged re-Add
// must not call Embed, a changed one must.
type countingEmbedder struct {
	calls int
}

func (c *countingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	c.calls++
	// Any deterministic mapping works; reuse the letter-count scheme so similar
	// text stays similar (not that this test ranks on it).
	var vec [4]float32
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

// TestModelTagInvalidatesCacheAcrossModelChange is the regression for the
// mixed-vector-space bug: a persistent index (a real file) is built with one
// embedding model, then re-opened under a DIFFERENT model. An unchanged symbol must
// NOT reuse the first model's vector — the content-hash key folds in the model tag,
// so the re-Add is a cache MISS and re-embeds. Without this, unchanged symbols keep
// OLD-model vectors while new symbols get NEW-model vectors, mixing two spaces (and
// possibly dimensions) in one HNSW graph.
func TestModelTagInvalidatesCacheAcrossModelChange(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sem.db")

	// Build with model "m1": one embed for the symbol.
	emb1 := &countingEmbedder{}
	ix1, err := OpenModel(dbPath, emb1, "model-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix1.Add(ctx, "Sym", "abcd"); err != nil {
		t.Fatalf("Add under model-1: %v", err)
	}
	if emb1.calls != 1 {
		t.Fatalf("model-1 first Add embed calls = %d, want 1", emb1.calls)
	}
	// Same model, unchanged text: cache hit (sanity that the model tag does not break
	// the normal same-model cache).
	if err := ix1.Add(ctx, "Sym", "abcd"); err != nil {
		t.Fatalf("re-Add under model-1: %v", err)
	}
	if emb1.calls != 1 {
		t.Fatalf("same-model unchanged re-Add embed calls = %d, want 1 (cache should hit)", emb1.calls)
	}
	ix1.Close()

	// Re-open the SAME file under model "m2". The unchanged symbol must re-embed,
	// because its stored hash was computed with the model-1 tag and now MISSES.
	emb2 := &countingEmbedder{}
	ix2, err := OpenModel(dbPath, emb2, "model-2")
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	if err := ix2.Add(ctx, "Sym", "abcd"); err != nil {
		t.Fatalf("Add under model-2: %v", err)
	}
	if emb2.calls != 1 {
		t.Fatalf("after model change, embed calls = %d, want 1 (a stale-model vector was reused)", emb2.calls)
	}
}

// TestContentHashCacheSkipsUnchangedReAdd is the D2-T01 acceptance test: the
// first Add embeds, an unchanged re-Add reuses the cached vector (no Embed
// call), and a changed re-Add re-embeds.
func TestContentHashCacheSkipsUnchangedReAdd(t *testing.T) {
	ctx := context.Background()
	emb := &countingEmbedder{}
	ix := openIndex(t, emb)

	// First Add: must embed once.
	if err := ix.Add(ctx, "doc", "abcd"); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("after first Add, embed calls = %d, want 1", emb.calls)
	}

	// Unchanged re-Add: must NOT embed (cache hit on identical text).
	if err := ix.Add(ctx, "doc", "abcd"); err != nil {
		t.Fatalf("unchanged re-Add: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("after unchanged re-Add, embed calls = %d, want 1 (cache miss?)", emb.calls)
	}

	// Changed re-Add: text differs, must re-embed.
	if err := ix.Add(ctx, "doc", "abcde"); err != nil {
		t.Fatalf("changed re-Add: %v", err)
	}
	if emb.calls != 2 {
		t.Fatalf("after changed re-Add, embed calls = %d, want 2", emb.calls)
	}

	// Re-Add the new text again: cache hit, no further embed.
	if err := ix.Add(ctx, "doc", "abcde"); err != nil {
		t.Fatalf("second unchanged re-Add: %v", err)
	}
	if emb.calls != 2 {
		t.Fatalf("after second unchanged re-Add, embed calls = %d, want 2", emb.calls)
	}
}

// TestContentHashCacheNullHashReEmbedsOnce proves the migration path: a row
// written without a hash (as a pre-D2-T01 database would have) re-embeds exactly
// once on its next Add, after which it is cached normally.
func TestContentHashCacheNullHashReEmbedsOnce(t *testing.T) {
	ctx := context.Background()
	emb := &countingEmbedder{}
	ix := openIndex(t, emb)

	// Simulate a legacy row: vector present, hash NULL. Insert directly so Add's
	// hashing logic does not run.
	if _, err := ix.db.ExecContext(ctx,
		`INSERT INTO docs (id, text, vec, hash) VALUES (?, ?, ?, NULL)`,
		"legacy", "abcd", "[1,1,1,1]"); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if emb.calls != 0 {
		t.Fatalf("seeding must not embed, got %d", emb.calls)
	}

	// First Add over the legacy id: null hash forces a re-embed even though the
	// text is unchanged.
	if err := ix.Add(ctx, "legacy", "abcd"); err != nil {
		t.Fatalf("re-Add legacy: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("legacy re-Add embed calls = %d, want 1", emb.calls)
	}

	// Now the hash is stored: a second unchanged Add is a cache hit.
	if err := ix.Add(ctx, "legacy", "abcd"); err != nil {
		t.Fatalf("second re-Add legacy: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("after legacy hash stored, embed calls = %d, want 1", emb.calls)
	}
}

// tableEmbedder maps exact text strings to precomputed vectors. It lets the HNSW
// tests drive the index purely through the public text API while controlling the
// underlying vectors precisely.
type tableEmbedder struct {
	table map[string][]float32
}

func (e *tableEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v, ok := e.table[text]
	if !ok {
		return nil, fmt.Errorf("tableEmbedder: no vector for %q", text)
	}
	return v, nil
}

// randomVec draws a deterministic random vector in [-1,1]^dim.
func randomVec(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(rng.Float64()*2 - 1)
	}
	return v
}

// clusteredVec draws a vector near one of nClusters fixed centers, modelling the
// manifold structure real embeddings have (purely uniform high-dim noise is the
// pathological worst case for any ANN index and is not representative). center
// selects the cluster; spread controls jitter around it.
func clusteredVec(rng *rand.Rand, centers [][]float32, center int, spread float64) []float32 {
	c := centers[center]
	v := make([]float32, len(c))
	for i := range v {
		v[i] = c[i] + float32(rng.NormFloat64()*spread)
	}
	return v
}

// exactTopK is the ground-truth brute-force cosine ranking, used to measure HNSW
// recall. Returns the ids of the k nearest vectors to query, nearest first.
func exactTopK(query []float32, ids []string, vecs [][]float32, k int) []string {
	type sc struct {
		id   string
		dist float64
	}
	scored := make([]sc, len(ids))
	for i := range ids {
		scored[i] = sc{id: ids[i], dist: cosineDist(query, vecs[i])}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].dist != scored[j].dist {
			return scored[i].dist < scored[j].dist
		}
		return scored[i].id < scored[j].id
	})
	if k > len(scored) {
		k = len(scored)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = scored[i].id
	}
	return out
}

// TestHNSWRecallAndSublinear is the D2-T02 acceptance test. Over a ≥200-vector
// fixture it asserts (a) HNSW recall ≥ 0.9 against an exact cosine scan and
// (b) that vector search is sub-linear — it visits far fewer than N graph nodes.
func TestHNSWRecallAndSublinear(t *testing.T) {
	ctx := context.Background()

	const (
		n         = 1000
		dim       = 32
		k         = 10
		queries   = 50
		nClusters = 20
	)
	rng := rand.New(rand.NewSource(42))

	// Cluster centers spread the corpus across the space; documents jitter around
	// a center. This mirrors how real embeddings sit on a manifold.
	centers := make([][]float32, nClusters)
	for c := range centers {
		centers[c] = randomVec(rng, dim)
	}

	ids := make([]string, n)
	vecs := make([][]float32, n)
	table := make(map[string][]float32, n+queries)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("doc%04d", i)
		v := clusteredVec(rng, centers, i%nClusters, 0.15)
		ids[i] = id
		vecs[i] = v
		table[id] = v
	}

	// Query texts map to vectors jittered near a cluster center too.
	queryTexts := make([]string, queries)
	for j := 0; j < queries; j++ {
		qt := fmt.Sprintf("query%04d", j)
		queryTexts[j] = qt
		table[qt] = clusteredVec(rng, centers, j%nClusters, 0.15)
	}

	ix := openIndex(t, &tableEmbedder{table: table})
	for i := 0; i < n; i++ {
		if err := ix.Add(ctx, ids[i], ids[i]); err != nil {
			t.Fatalf("Add(%q): %v", ids[i], err)
		}
	}

	var totalFound, totalWanted int
	var maxVisited int
	for _, qt := range queryTexts {
		hits, err := ix.Search(ctx, qt, k)
		if err != nil {
			t.Fatalf("Search(%q): %v", qt, err)
		}
		// Hits must be score-descending (preserved public contract).
		for i := 1; i < len(hits); i++ {
			if hits[i-1].Score < hits[i].Score {
				t.Fatalf("hits not score-descending for %q: %+v", qt, hits)
			}
		}
		got := make(map[string]bool, len(hits))
		for _, h := range hits {
			got[h.ID] = true
		}
		want := exactTopK(table[qt], ids, vecs, k)
		for _, id := range want {
			if got[id] {
				totalFound++
			}
		}
		totalWanted += len(want)

		if ix.lastVisited > maxVisited {
			maxVisited = ix.lastVisited
		}
	}

	recall := float64(totalFound) / float64(totalWanted)
	if recall < 0.9 {
		t.Errorf("HNSW recall@%d = %.3f, want >= 0.9", k, recall)
	}

	// Sub-linearity: a search must not touch the whole corpus. With n=300 the
	// graph should resolve top-k while visiting well under half the nodes. We use
	// the worst (max) visited count across all queries as the evidence.
	if maxVisited == 0 {
		t.Fatal("lastVisited never recorded; search did not run")
	}
	if maxVisited >= n {
		t.Errorf("search visited %d of %d nodes — not sub-linear", maxVisited, n)
	}
	if maxVisited > n/2 {
		t.Errorf("search visited %d of %d nodes (> n/2) — weaker sub-linearity than expected", maxVisited, n)
	}
	t.Logf("HNSW recall@%d=%.3f, max visited=%d of %d nodes", k, recall, maxVisited, n)
}

// TestVectorSearchRebuildsOnAdd guards the lazy-rebuild path: documents added
// after the first vector Search must still be searchable (the dirty flag forces
// a graph rebuild), and the cache must not strand them out of the graph.
func TestVectorSearchRebuildsOnAdd(t *testing.T) {
	ctx := context.Background()
	table := map[string][]float32{
		"a": {1, 0, 0},
		"b": {0, 1, 0},
		"c": {0, 0, 1},
		"q": {0, 0, 1},
	}
	ix := openIndex(t, &tableEmbedder{table: table})

	if err := ix.Add(ctx, "a", "a"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Add(ctx, "b", "b"); err != nil {
		t.Fatal(err)
	}
	// First search builds the graph over {a, b}.
	if _, err := ix.Search(ctx, "q", 3); err != nil {
		t.Fatalf("first Search: %v", err)
	}
	// Add a third doc that is the exact match for q.
	if err := ix.Add(ctx, "c", "c"); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search(ctx, "q", 1)
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "c" {
		t.Fatalf("after rebuild, top hit = %+v, want c", hits)
	}
}
