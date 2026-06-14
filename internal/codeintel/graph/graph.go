// Package graph is the code graph (P3-T10): nodes (symbols/files) and edges
// (calls, references, defines, implements, …) in SQLite, with structural queries
// — callers/callees and transitive reachability/closure via recursive CTEs. This
// is the backbone pure-RAG lacks: structure, not just text. Builds are idempotent
// (INSERT OR IGNORE), so re-indexing a file never duplicates.
package graph

import (
	"context"
	"database/sql"
	"fmt"

	"nilcore/internal/codeintel/ast"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
    id   TEXT PRIMARY KEY,
    kind TEXT,
    name TEXT,
    file TEXT
);
CREATE TABLE IF NOT EXISTS edges (
    from_id TEXT NOT NULL,
    to_id   TEXT NOT NULL,
    kind    TEXT NOT NULL,
    UNIQUE(from_id, to_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id, kind);
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id, kind);`

// Node is a symbol or file.
type Node struct {
	ID   string
	Kind string
	Name string
	File string
}

// Edge connects two nodes with a relationship kind.
type Edge struct {
	From string
	To   string
	Kind string
}

// Graph is a SQLite-backed code graph.
type Graph struct {
	db *sql.DB
}

// Open opens (creating if needed) a graph at path (use ":memory:" for ephemeral).
func Open(path string) (*Graph, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open graph: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("graph schema: %w", err)
	}
	return &Graph{db: db}, nil
}

// Close closes the graph.
func (g *Graph) Close() error { return g.db.Close() }

// AddNode inserts a node (idempotent).
func (g *Graph) AddNode(ctx context.Context, n Node) error {
	_, err := g.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO nodes (id, kind, name, file) VALUES (?, ?, ?, ?)`,
		n.ID, n.Kind, n.Name, n.File)
	return err
}

// AddEdge inserts an edge (idempotent).
func (g *Graph) AddEdge(ctx context.Context, e Edge) error {
	_, err := g.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO edges (from_id, to_id, kind) VALUES (?, ?, ?)`,
		e.From, e.To, e.Kind)
	return err
}

// BuildFile indexes a Go file: its symbols become nodes and its calls become
// `calls` edges (by name, scoped to the fixture/package). Idempotent.
func (g *Graph) BuildFile(ctx context.Context, path string) error {
	syms, err := ast.Symbols(path)
	if err != nil {
		return err
	}
	for _, s := range syms {
		if err := g.AddNode(ctx, Node{ID: s.Name, Kind: string(s.Kind), Name: s.Name, File: path}); err != nil {
			return err
		}
	}
	calls, err := ast.Calls(path)
	if err != nil {
		return err
	}
	for caller, callees := range calls {
		for _, callee := range callees {
			if err := g.AddEdge(ctx, Edge{From: caller, To: callee, Kind: "calls"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Callers returns the direct callers of id.
func (g *Graph) Callers(ctx context.Context, id string) ([]string, error) {
	return g.neighbors(ctx, `SELECT from_id FROM edges WHERE to_id = ? AND kind = 'calls' ORDER BY from_id`, id)
}

// Callees returns the direct callees of id.
func (g *Graph) Callees(ctx context.Context, id string) ([]string, error) {
	return g.neighbors(ctx, `SELECT to_id FROM edges WHERE from_id = ? AND kind = 'calls' ORDER BY to_id`, id)
}

// Closure returns every node transitively reachable from id along `calls` edges.
func (g *Graph) Closure(ctx context.Context, id string) ([]string, error) {
	const q = `
WITH RECURSIVE reach(id) AS (
    SELECT to_id FROM edges WHERE from_id = ? AND kind = 'calls'
    UNION
    SELECT e.to_id FROM edges e JOIN reach r ON e.from_id = r.id WHERE e.kind = 'calls'
)
SELECT id FROM reach ORDER BY id`
	return g.neighbors(ctx, q, id)
}

// Reachable reports whether `to` is transitively reachable from `from`.
func (g *Graph) Reachable(ctx context.Context, from, to string) (bool, error) {
	ids, err := g.Closure(ctx, from)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == to {
			return true, nil
		}
	}
	return false, nil
}

func (g *Graph) neighbors(ctx context.Context, q, arg string) ([]string, error) {
	rows, err := g.db.QueryContext(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
