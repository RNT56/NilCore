// Package graph is the code graph (P3-T10): nodes (symbols) and edges in SQLite,
// with structural queries — callers/callees and transitive reachability/closure
// via recursive CTEs. This is the backbone pure-RAG lacks: structure, not just
// text. Two edge kinds ship today: `calls` (caller-name → callee-name, the call
// graph the structural queries traverse) and `references` (file → each identifier
// it uses, the tag map). The `kind` column is free-text, so richer kinds
// (implements, imports, inherits, …) can slot in without a schema change. Builds
// are idempotent (INSERT OR IGNORE), so re-indexing a file never duplicates.
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
	// Pin to a single connection (mirrors internal/store). This is REQUIRED for a
	// ":memory:" graph — each pooled connection gets its OWN private in-memory database,
	// so a second connection would see an empty schema; it also serializes writers on a
	// file-backed graph, avoiding SQLITE_BUSY.
	db.SetMaxOpenConns(1)
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

// BuildFile (re)indexes a source file: its symbols become nodes, its calls become
// `calls` edges (by name, scoped to the fixture/package), and its references — the
// "tag map" of identifiers it uses — become `references` edges from the file to
// each referenced name. It is a full REPLACE of the file's contribution, not an
// append — it first prunes the file's prior nodes and every edge originating from
// the file (the edges from its symbols AND the file's own `references` edges), so
// a symbol, call, or reference the file no longer contains does NOT linger (the
// incremental `live` re-index depends on this). Symbols are upserted, so a
// symbol's file/kind is refreshed on rebuild (e.g. when it moves into this file).
// The prune+rebuild is atomic in one transaction.
func (g *Graph) BuildFile(ctx context.Context, path string) error {
	syms, err := ast.Symbols(path)
	if err != nil {
		return err
	}
	calls, err := ast.Calls(path)
	if err != nil {
		return err
	}
	refs, err := ast.References(path)
	if err != nil {
		return err
	}

	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Prune the file's prior contribution: the edges that originate from its
	// symbols, the file's own `references` edges (keyed by the file path, so they
	// are not caught by the node-scoped delete), then its nodes. Edges INTO this
	// file's symbols from elsewhere are owned by the caller's file and are
	// deliberately left intact.
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE file = ?)`, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE from_id = ? AND kind = 'references'`, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE file = ?`, path); err != nil {
		return err
	}

	for _, s := range syms {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (id, kind, name, file) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, name=excluded.name, file=excluded.file`,
			s.Name, string(s.Kind), s.Name, path); err != nil {
			return err
		}
	}
	for caller, callees := range calls {
		for _, callee := range callees {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO edges (from_id, to_id, kind) VALUES (?, ?, 'calls')`,
				caller, callee); err != nil {
				return err
			}
		}
	}
	// References (the tag map): a `references` edge from the file to each name it
	// uses. The file path is the edge source (not a symbol node) because the flat
	// reference list is not attributed to an owning symbol; INSERT OR IGNORE
	// collapses the many duplicate uses of a name into one edge.
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO edges (from_id, to_id, kind) VALUES (?, ?, 'references')`,
			path, ref.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveFile drops a file's entire contribution from the graph: its symbol nodes,
// every edge originating from those symbols, the file's own `references` edges, and
// — unlike BuildFile — every edge pointing INTO those symbols from elsewhere. It is
// the deletion/rename counterpart to BuildFile: BuildFile re-indexes a file that
// still exists (and deliberately keeps incoming edges, since the file lives on),
// whereas RemoveFile is for a path that is gone, so leaving incoming edges would
// dangle them at a node that no longer exists. The whole removal is atomic in one
// transaction; removing an unknown path is a clean no-op.
func (g *Graph) RemoveFile(ctx context.Context, path string) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove file: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Edges out of, then into, the file's symbols (resolved before the nodes go).
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE file = ?)`, path); err != nil {
		return fmt.Errorf("remove out-edges of %q: %w", path, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE to_id IN (SELECT id FROM nodes WHERE file = ?)`, path); err != nil {
		return fmt.Errorf("remove in-edges of %q: %w", path, err)
	}
	// The file's own `references` edges (keyed by the file path, not a node id).
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE from_id = ? AND kind = 'references'`, path); err != nil {
		return fmt.Errorf("remove reference edges of %q: %w", path, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE file = ?`, path); err != nil {
		return fmt.Errorf("remove nodes of %q: %w", path, err)
	}
	return tx.Commit()
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

// Nodes returns all nodes (for whole-graph algorithms like PageRank).
func (g *Graph) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := g.db.QueryContext(ctx, `SELECT id, kind, name, file FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.File); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Edges returns all edges (optionally filter by kind with kind != "").
func (g *Graph) Edges(ctx context.Context, kind string) ([]Edge, error) {
	q := `SELECT from_id, to_id, kind FROM edges`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY from_id, to_id`
	rows, err := g.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.To, &e.Kind); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
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
