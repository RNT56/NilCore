// Package semantic is the hybrid, degradable semantic index (P3-T13): it stores
// symbol text in SQLite and — when an Embedder is wired in — its vector, ranking
// search by cosine similarity. The point is graceful degradation: an Index with a
// nil Embedder is still useful, falling back to lexical term-overlap scoring so
// the agent never loses retrieval entirely just because no embedding provider is
// configured. Vectors are JSON-encoded in a single column (NULL when absent), so
// the schema stays one table and the build stays cgo-free.
//
// Two refinements layer on that base. A content-hash cache (D2-T01) records the
// sha256 of each document's text so an unchanged re-Add reuses the stored vector
// instead of paying for another Embedder call. And vector search is served by an
// in-memory HNSW graph (D2-T02) built from the stored vectors, replacing the old
// brute-force linear scan; the graph is rebuilt lazily whenever the doc set
// changes. Both are pure stdlib + the sanctioned SQLite driver — no cgo.
package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// schema is created on Open. The hash column (D2-T01) is additive and nullable:
// older databases predate it, so Open also runs an idempotent ALTER to add it to
// an existing table. A null hash forces a one-time re-embed on the next Add.
const schema = `
CREATE TABLE IF NOT EXISTS docs (
    id   TEXT PRIMARY KEY,
    text TEXT NOT NULL,
    vec  TEXT,
    hash TEXT
);`

// Embedder turns text into a vector. It is provider-agnostic: any model client
// that can map text to a fixed-length []float32 satisfies it. A nil Embedder is
// the explicit signal to degrade to lexical search.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Hit is a search result: a document id and its relevance score (higher is more
// relevant, for both vector and lexical modes).
type Hit struct {
	ID    string
	Score float64
}

// Index is a SQLite-backed semantic index. When emb is non-nil it ranks by cosine
// similarity (served by an HNSW graph); otherwise it ranks lexically.
//
// The HNSW graph is an in-memory cache derived from the stored vectors. It is
// built lazily on the first vector Search and invalidated (dirty=true) whenever
// Add mutates the doc set, so the next Search rebuilds it. mu guards the lazy
// build/rebuild against concurrent Search calls.
type Index struct {
	db  *sql.DB
	emb Embedder

	mu    sync.Mutex
	graph *hnswGraph
	dirty bool // graph is stale (doc set changed) and must be rebuilt

	// lastVisited records how many graph nodes the most recent vector search
	// touched. It exists so tests can prove the search is sub-linear in N; it
	// is not part of the public API.
	lastVisited int
}

// Open opens (creating if needed) an index at path (use ":memory:" for ephemeral).
// Pass a nil Embedder to run in lexical-only mode.
func Open(path string, e Embedder) (*Index, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open semantic index: %w", err)
	}
	// Pin to a single connection (mirrors internal/store). REQUIRED for ":memory:"
	// (each pooled connection is a separate private in-memory DB) and serializes
	// writers on a file-backed index, avoiding SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic schema: %w", err)
	}
	// Bring pre-D2-T01 databases up to the current schema. SQLite has no
	// IF NOT EXISTS for ADD COLUMN, so a duplicate-column error here is benign
	// and means the column already exists.
	if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN hash TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		db.Close()
		return nil, fmt.Errorf("semantic schema migrate: %w", err)
	}
	// An index opened against existing data must build its graph on first use.
	return &Index{db: db, emb: e, dirty: true}, nil
}

// Close closes the index.
func (ix *Index) Close() error { return ix.db.Close() }

// Add stores a document's text under id (replacing any prior row for that id). If
// an Embedder is configured, the text's embedding is computed and stored too.
//
// Content-hash cache (D2-T01): Add records sha256(text). When re-Adding an id
// whose text is unchanged (stored hash matches) and whose vector is already
// present, the stored vector is reused and Embedder.Embed is NOT called — this
// is the common "re-index an unchanged file" path. Changed text re-embeds and
// replaces; rows written before this column existed have a null hash and so
// re-embed exactly once on their next Add.
func (ix *Index) Add(ctx context.Context, id, text string) error {
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])

	var vec any // NULL when no embedder
	if ix.emb != nil {
		// Reuse the cached vector iff the text is byte-identical (hash match) and
		// a vector actually exists for the row.
		if cached, ok, err := ix.cachedVector(ctx, id, hash); err != nil {
			return err
		} else if ok {
			vec = cached
		} else {
			v, err := ix.emb.Embed(ctx, text)
			if err != nil {
				return fmt.Errorf("embed %q: %w", id, err)
			}
			encoded, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("encode vector for %q: %w", id, err)
			}
			vec = string(encoded)
		}
	}
	// Hold mu across BOTH the row write and the dirty-set so they are atomic with
	// respect to a concurrent Search (which takes mu before consulting the graph):
	// a Search either runs entirely before this block — consistent with the old doc
	// set — or sees dirty==true after it and rebuilds with the new row. Without the
	// shared lock a Search could observe the committed row while dirty was still
	// false and omit it. (The only shipped caller indexes single-threaded, but this
	// keeps Index correct for any concurrent Add+Search.)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, err := ix.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO docs (id, text, vec, hash) VALUES (?, ?, ?, ?)`,
		id, text, vec, hash); err != nil {
		return fmt.Errorf("add %q: %w", id, err)
	}
	ix.dirty = true // the doc set changed; the vector graph (if any) is now stale
	return nil
}

// IDs returns every document id currently stored, sorted ascending. It exists so
// a persistent index can be reconciled against the live symbol set: an id present
// here but absent from the live set is stale (a renamed/deleted symbol) and can be
// pruned via Delete, keeping the cross-run index from growing unbounded.
func (ix *Index) IDs(ctx context.Context) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT id FROM docs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}

// Delete removes the document stored under id (a no-op if none exists). Deleting a
// row changes the doc set, so the vector graph is marked stale and rebuilds on the
// next vector Search — mirroring Add's dirty-set, under the same lock so a
// concurrent Search either predates the delete or rebuilds without the row.
func (ix *Index) Delete(ctx context.Context, id string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, err := ix.db.ExecContext(ctx, `DELETE FROM docs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete %q: %w", id, err)
	}
	ix.dirty = true
	return nil
}

// cachedVector returns the stored vector JSON for id when the row's stored hash
// equals newHash and a non-null vector exists, signalling Add can skip Embed.
// ok is false (no error) when there is no row, no stored hash, a hash mismatch,
// or no stored vector — every case that requires a fresh embed.
func (ix *Index) cachedVector(ctx context.Context, id, newHash string) (string, bool, error) {
	var storedHash, storedVec sql.NullString
	err := ix.db.QueryRowContext(ctx,
		`SELECT hash, vec FROM docs WHERE id = ?`, id).Scan(&storedHash, &storedVec)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lookup cache for %q: %w", id, err)
	}
	if storedHash.Valid && storedVec.Valid && storedHash.String == newHash {
		return storedVec.String, true, nil
	}
	return "", false, nil
}

// Search returns the top-k most relevant documents for query, score descending;
// the returned hits are ordered deterministically (score, then id). With an
// Embedder it ranks by cosine similarity via the HNSW graph — an APPROXIMATE
// index, so under many exactly-equidistant vectors the returned k may be any
// stable subset of the tied group, not necessarily the id-smallest; widen
// efSearch if exact tie selection ever matters. Without an Embedder it ranks by
// case-insensitive term overlap of the query against each document's text (exact).
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	if k <= 0 {
		return nil, nil
	}
	var hits []Hit
	var err error
	if ix.emb != nil {
		hits, err = ix.searchVector(ctx, query, k)
	} else {
		hits, err = ix.searchLexical(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

// searchVector embeds the query and asks the HNSW graph for the k nearest stored
// vectors. The graph is (re)built lazily here when stale, so callers pay the
// build cost only on the first search after an Add — never on every Add. Search
// then walks the graph instead of scanning every row, visiting far fewer than N
// nodes; the visited count is recorded in ix.lastVisited for sub-linearity tests.
//
// k is needed up front (unlike the old linear scan) because HNSW search is
// top-k by construction; passing it down keeps the graph from over-collecting.
func (ix *Index) searchVector(ctx context.Context, query string, k int) ([]Hit, error) {
	q, err := ix.emb.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.graph == nil || ix.dirty {
		g, err := ix.buildGraph(ctx)
		if err != nil {
			return nil, err
		}
		ix.graph = g
		ix.dirty = false
	}

	hits, visited := ix.graph.search(q, k)
	ix.lastVisited = visited
	return hits, nil
}

// buildGraph loads every stored vector from SQLite and constructs a fresh HNSW
// graph over them. Caller holds ix.mu.
func (ix *Index) buildGraph(ctx context.Context) (*hnswGraph, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT id, vec FROM docs WHERE vec IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("scan vectors: %w", err)
	}
	defer rows.Close()

	var ids []string
	var vecs [][]float32
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan vector row: %w", err)
		}
		var v []float32
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("decode vector for %q: %w", id, err)
		}
		ids = append(ids, id)
		vecs = append(vecs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vectors: %w", err)
	}
	return newHNSW(ids, vecs), nil
}

// searchLexical scores every document by the fraction of query terms that appear
// (case-insensitively, as substrings) in its text. Documents with no overlap are
// dropped so callers see only relevant hits.
func (ix *Index) searchLexical(ctx context.Context, query string) ([]Hit, error) {
	terms := terms(query)
	rows, err := ix.db.QueryContext(ctx, `SELECT id, text FROM docs`)
	if err != nil {
		return nil, fmt.Errorf("scan docs: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, fmt.Errorf("scan doc row: %w", err)
		}
		if score := overlap(terms, text); score > 0 {
			hits = append(hits, Hit{ID: id, Score: score})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate docs: %w", err)
	}
	return hits, nil
}

// terms splits a query into lowercased, whitespace-separated terms.
func terms(query string) []string {
	return strings.Fields(strings.ToLower(query))
}

// overlap is the fraction of query terms found (case-insensitively) in text.
func overlap(terms []string, text string) float64 {
	if len(terms) == 0 {
		return 0
	}
	lower := strings.ToLower(text)
	var found int
	for _, t := range terms {
		if strings.Contains(lower, t) {
			found++
		}
	}
	return float64(found) / float64(len(terms))
}
