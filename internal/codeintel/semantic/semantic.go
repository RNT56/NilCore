// Package semantic is the hybrid, degradable semantic index (P3-T13): it stores
// symbol text in SQLite and — when an Embedder is wired in — its vector, ranking
// search by cosine similarity. The point is graceful degradation: an Index with a
// nil Embedder is still useful, falling back to lexical term-overlap scoring so
// the agent never loses retrieval entirely just because no embedding provider is
// configured. Vectors are JSON-encoded in a single column (NULL when absent), so
// the schema stays one table and the build stays cgo-free.
package semantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS docs (
    id   TEXT PRIMARY KEY,
    text TEXT NOT NULL,
    vec  TEXT
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
// similarity; otherwise it ranks lexically.
type Index struct {
	db  *sql.DB
	emb Embedder
}

// Open opens (creating if needed) an index at path (use ":memory:" for ephemeral).
// Pass a nil Embedder to run in lexical-only mode.
func Open(path string, e Embedder) (*Index, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open semantic index: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("semantic schema: %w", err)
	}
	return &Index{db: db, emb: e}, nil
}

// Close closes the index.
func (ix *Index) Close() error { return ix.db.Close() }

// Add stores a document's text under id (replacing any prior row for that id). If
// an Embedder is configured, the text's embedding is computed and stored too.
func (ix *Index) Add(ctx context.Context, id, text string) error {
	var vec any // NULL when no embedder
	if ix.emb != nil {
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
	if _, err := ix.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO docs (id, text, vec) VALUES (?, ?, ?)`,
		id, text, vec); err != nil {
		return fmt.Errorf("add %q: %w", id, err)
	}
	return nil
}

// Search returns the top-k most relevant documents for query, score descending,
// with a deterministic tie-break by id. With an Embedder it ranks by cosine
// similarity of the query embedding against stored vectors; without one it ranks
// by case-insensitive term overlap of the query against each document's text.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	if k <= 0 {
		return nil, nil
	}
	var hits []Hit
	var err error
	if ix.emb != nil {
		hits, err = ix.searchVector(ctx, query)
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

// searchVector scores every document with a stored vector by cosine similarity.
func (ix *Index) searchVector(ctx context.Context, query string) ([]Hit, error) {
	q, err := ix.emb.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT id, vec FROM docs WHERE vec IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("scan vectors: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan vector row: %w", err)
		}
		var v []float32
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("decode vector for %q: %w", id, err)
		}
		hits = append(hits, Hit{ID: id, Score: cosine(q, v)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vectors: %w", err)
	}
	return hits, nil
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

// cosine is the cosine similarity of two vectors; mismatched or zero-magnitude
// vectors score 0.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
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
