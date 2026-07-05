// Package graph is the code graph (P3-T10): nodes (symbols) and edges in SQLite,
// with structural queries — callers/callees and transitive reachability/closure
// via recursive CTEs. This is the backbone pure-RAG lacks: structure, not just
// text. Two edge kinds ship today: `calls` (caller → callee, the call graph the
// structural queries traverse) and `references` (file → each identifier it uses,
// the tag map). The `kind` column is free-text, so richer kinds (implements,
// imports, inherits, …) can slot in without a schema change. Builds are idempotent
// (INSERT OR IGNORE), so re-indexing a file never duplicates.
//
// # Qualified node ids and the name-resolution layer
//
// Node ids are QUALIFIED: NodeID(file, recv, name) = "file\x00recv\x00name". This
// is the fix for the same-name collision bug — two files that each declare `Run`,
// or a method `Close` on two receivers, are now DISTINCT nodes, so indexing one
// never clobbers the other. The bare `name` still lives in the node's Name column
// for display and for resolution.
//
// Cross-file call resolution is inherently NAME-based: a call site emits the bare
// callee name (`helper()`), not the file that defines it. BuildFile therefore stores
// a `calls` edge as (qualifiedCaller → bareCalleeName) and the query layer resolves
// that bare name to the matching qualified node(s) at read time. The public query
// APIs (Callees/Callers/Closure/Reachable) accept EITHER a qualified id or a bare
// name and return qualified ids: a bare-name argument resolves to every qualified
// node with that name, and a bare `to_id` on an edge resolves the same way. Where a
// bare name is ambiguous across files (two `Run`s), an edge into/out of it fans out
// to ALL matching qualified nodes rather than silently dropping the edge — an
// over-approximation that is safe for the callers/impact/dead-code lenses (running
// an extra test or reviewing an extra symbol never breaks correctness; missing one
// could). The heuristic is documented at resolveName.
package graph

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

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
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);
CREATE TABLE IF NOT EXISTS edges (
    from_id TEXT NOT NULL,
    to_id   TEXT NOT NULL,
    kind    TEXT NOT NULL,
    UNIQUE(from_id, to_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id, kind);
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id, kind);`

// idSep separates the (file, receiver, name) components of a qualified node id. A
// NUL byte can appear in neither a filesystem path nor a source identifier, so it is
// an unambiguous delimiter — a qualified id round-trips through SplitID without loss.
const idSep = "\x00"

// NodeID builds the qualified id for a symbol: file, receiver (empty for a free
// function/type/var/const), and bare name, NUL-joined. Same-named symbols in
// different files, or a method on two receivers, get DISTINCT ids — the whole point
// of the qualified scheme. The file is used as-is (callers pass whatever path they
// index with — absolute for the tools' worktree walk, temp paths in tests); it only
// has to be stable within one graph for two symbols to compare equal.
func NodeID(file, recv, name string) string {
	return file + idSep + recv + idSep + name
}

// SplitID inverts NodeID into (file, recv, name). A bare name with no separators
// (e.g. a directly-AddNode'd or AddEdge'd literal id from a caller that predates the
// qualified scheme) round-trips as ("", "", id) so legacy/literal ids stay usable.
func SplitID(id string) (file, recv, name string) {
	parts := strings.SplitN(id, idSep, 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		return "", "", id
	}
}

// DisplayName renders a qualified node id as the human-facing symbol name: the bare
// name for a free function/type/var/const, or "recv.name" for a method. It is what a
// consumer shows a user — NEVER the qualified id, which embeds the (possibly absolute)
// file path and NUL delimiters. A literal/legacy id with no separators renders as
// itself. This is the display-side counterpart to SplitID: identity/dedup stays on the
// full qualified id, but everything user-visible goes through here.
func DisplayName(id string) string {
	_, recv, name := SplitID(id)
	if recv != "" {
		return recv + "." + name
	}
	return name
}

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

// AddNode inserts a node (idempotent). The id is used verbatim — callers building
// from source pass NodeID(...); low-level callers (and tests) may pass any stable
// literal id.
func (g *Graph) AddNode(ctx context.Context, n Node) error {
	_, err := g.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO nodes (id, kind, name, file) VALUES (?, ?, ?, ?)`,
		n.ID, n.Kind, n.Name, n.File)
	return err
}

// AddEdge inserts an edge (idempotent). Endpoints are used verbatim.
func (g *Graph) AddEdge(ctx context.Context, e Edge) error {
	_, err := g.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO edges (from_id, to_id, kind) VALUES (?, ?, ?)`,
		e.From, e.To, e.Kind)
	return err
}

// BuildFile (re)indexes a source file: its symbols become QUALIFIED nodes
// (NodeID(file, recv, name)), its calls become `calls` edges from the qualified
// caller to the BARE callee name (resolved to qualified nodes at query time; see the
// package doc), and its references — the "tag map" of identifiers it uses — become
// `references` edges from the file to each referenced name. It is a full REPLACE of
// the file's contribution, not an append — it first prunes the file's prior nodes and
// every edge originating from the file (the edges from its symbols AND the file's own
// `references` edges), so a symbol, call, or reference the file no longer contains
// does NOT linger (the incremental `live` re-index depends on this). Symbols are
// upserted, so a symbol's file/kind is refreshed on rebuild. The prune+rebuild is
// atomic in one transaction.
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

	// Map each bare caller name in this file to its qualified id(s). ast.Calls keys by
	// bare function/method name (a method drops its receiver in the call map), so a
	// caller name may resolve to several qualified symbols in the same file (a method
	// on two receivers, or a method shadowing a free function). Emit the caller's
	// outgoing edges from EVERY matching qualified symbol so no outgoing call is
	// misattributed or lost.
	callerIDs := map[string][]string{}
	for _, s := range syms {
		if s.Kind == ast.KindFunc || s.Kind == ast.KindMethod {
			callerIDs[s.Name] = append(callerIDs[s.Name], NodeID(path, s.Recv, s.Name))
		}
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
		id := NodeID(path, s.Recv, s.Name)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (id, kind, name, file) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, name=excluded.name, file=excluded.file`,
			id, string(s.Kind), s.Name, path); err != nil {
			return err
		}
	}
	// `calls` edges: qualified caller → BARE callee name. The callee is left bare
	// because the call site names a function, not the file that defines it; the query
	// layer (resolveName) maps the bare callee to the matching qualified node(s) across
	// the whole graph at read time. A caller name that resolves to no symbol node (e.g.
	// a stray entry) still emits from the bare name so nothing is dropped.
	for caller, callees := range calls {
		froms := callerIDs[caller]
		if len(froms) == 0 {
			froms = []string{caller} // no matching symbol node: keep the bare caller id
		}
		for _, from := range froms {
			for _, callee := range callees {
				if _, err := tx.ExecContext(ctx,
					`INSERT OR IGNORE INTO edges (from_id, to_id, kind) VALUES (?, ?, 'calls')`,
					from, callee); err != nil {
					return err
				}
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
// dangle them at a node that no longer exists. Incoming `calls` edges are stored by
// BARE callee name (see BuildFile), so the in-edge prune matches on the bare NAME of
// the removed file's symbols, not their qualified ids. The whole removal is atomic in
// one transaction; removing an unknown path is a clean no-op.
func (g *Graph) RemoveFile(ctx context.Context, path string) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove file: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Edges out of the file's symbols (qualified from_id).
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE file = ?)`, path); err != nil {
		return fmt.Errorf("remove out-edges of %q: %w", path, err)
	}
	// Edges INTO the file's symbols. `calls` edges point at the bare callee NAME, so
	// match on the name of each symbol the file defines. Guard against removing an edge
	// whose bare name is ALSO defined by a surviving file: only prune a bare-name in-edge
	// when NO other file still defines that name (otherwise the edge is still live for
	// the survivor). Non-`calls` in-edges (should any exist) still match by qualified id.
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE to_id IN (SELECT id FROM nodes WHERE file = ?)`, path); err != nil {
		return fmt.Errorf("remove in-edges of %q (by id): %w", path, err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM edges
WHERE kind = 'calls'
  AND to_id IN (SELECT name FROM nodes WHERE file = ?)
  AND to_id NOT IN (SELECT name FROM nodes WHERE file <> ?)`, path, path); err != nil {
		return fmt.Errorf("remove in-edges of %q (by name): %w", path, err)
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

// resolveName maps a query argument to the set of qualified node ids it denotes. The
// argument may be:
//
//   - a qualified id that is itself a node (exact match) — returned as-is;
//   - a bare NAME — resolved to every node whose Name column equals it (the ambiguity
//     fan-out: two files' `Run` both match). This is where a bare callee/caller name
//     from an edge, or a bare name a consumer passes (retrieve leads, impact's changed
//     symbol, live's queried symbol), becomes the concrete qualified node(s);
//   - neither (a literal id with no node, e.g. a bare-seeded AddEdge endpoint whose
//     node was never AddNode'd) — returned verbatim as a single id so directly-seeded
//     edge graphs (tests, low-level callers) keep resolving to exactly what was seeded.
//
// The fan-out is deliberately an OVER-approximation on ambiguity: a bare name shared
// by N files expands to all N nodes rather than dropping the relationship. For the
// callers/callees/impact/dead-code lenses this is safe (an extra caller/test to review
// is harmless; a missed one is not). Same-file preference is applied by callers that
// have a file in hand (none currently need to narrow), so this stays a pure name map.
func (g *Graph) resolveName(ctx context.Context, arg string) ([]string, error) {
	// Exact node id?
	var exists int
	if err := g.db.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ? LIMIT 1`, arg).Scan(&exists); err == nil {
		return []string{arg}, nil
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	// Bare name → every qualified node with that name.
	rows, err := g.db.QueryContext(ctx, `SELECT id FROM nodes WHERE name = ? ORDER BY id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		return ids, nil
	}
	// Neither a node id nor a known name: a directly-seeded literal endpoint. Return it
	// verbatim so low-level edge graphs (AddEdge without AddNode) still traverse.
	return []string{arg}, nil
}

// Callers returns the direct callers of id (a qualified id or a bare name), as
// qualified ids. A `calls` edge stores the callee as a bare NAME (see BuildFile), so
// callers are found by matching the edge's to_id against the bare name of every node
// the argument resolves to.
func (g *Graph) Callers(ctx context.Context, id string) ([]string, error) {
	return g.adjacent(ctx, id, callDirCallers)
}

// Callees returns the direct callees of id (a qualified id or a bare name), as
// qualified ids. Each outgoing `calls` edge names a bare callee; that bare name is
// resolved to the matching qualified node(s), so a call to an ambiguous name fans out
// to every definition of it (the documented over-approximation).
func (g *Graph) Callees(ctx context.Context, id string) ([]string, error) {
	return g.adjacent(ctx, id, callDirCallees)
}

type callDir int

const (
	callDirCallees callDir = iota
	callDirCallers
)

// adjacent is the shared one-hop traversal for Callers/Callees. It resolves the
// argument to qualified source node(s), collects the raw neighbor endpoints from the
// `calls` edges in the requested direction, then maps each raw endpoint back into the
// qualified id space. Results are unique and sorted for determinism.
func (g *Graph) adjacent(ctx context.Context, id string, dir callDir) ([]string, error) {
	froms, err := g.resolveName(ctx, id)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, node := range froms {
		var raw []string
		switch dir {
		case callDirCallees:
			// Outgoing edges: from the qualified source node to bare callee names.
			raw, err = g.rawNeighbors(ctx, `SELECT to_id FROM edges WHERE from_id = ? AND kind = 'calls'`, node)
			if err != nil {
				return nil, err
			}
		case callDirCallers:
			// Incoming edges: a `calls` edge points at the callee's BARE name, so match on
			// the source node's name, not its qualified id.
			_, _, name := SplitID(node)
			raw, err = g.rawNeighbors(ctx, `SELECT from_id FROM edges WHERE to_id = ? AND kind = 'calls'`, name)
			if err != nil {
				return nil, err
			}
		}
		for _, r := range raw {
			ids, rerr := g.resolveName(ctx, r)
			if rerr != nil {
				return nil, rerr
			}
			for _, q := range ids {
				out[q] = true
			}
		}
	}
	return sortedKeys(out), nil
}

// Closure returns every node transitively reachable from id along `calls` edges, as
// qualified ids. Because a `calls` edge's callee is a bare name (resolved to qualified
// node(s) per hop), the traversal is done in Go over Callees rather than a single CTE
// — each hop must re-resolve the bare callee names to concrete nodes.
func (g *Graph) Closure(ctx context.Context, id string) ([]string, error) {
	seed, err := g.resolveName(ctx, id)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	queue := append([]string(nil), seed...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		callees, err := g.Callees(ctx, cur)
		if err != nil {
			return nil, err
		}
		for _, c := range callees {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	return sortedKeys(seen), nil
}

// Reachable reports whether `to` (a qualified id or a bare name) is transitively
// reachable from `from` along `calls` edges.
func (g *Graph) Reachable(ctx context.Context, from, to string) (bool, error) {
	ids, err := g.Closure(ctx, from)
	if err != nil {
		return false, err
	}
	targets, err := g.resolveName(ctx, to)
	if err != nil {
		return false, err
	}
	want := map[string]bool{}
	for _, t := range targets {
		want[t] = true
	}
	for _, id := range ids {
		if want[id] {
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

// Edges returns all edges (optionally filter by kind with kind != ""). Note that a
// `calls` edge's to_id is a bare callee NAME, not a qualified id — Edges returns the
// raw stored form; use Callees/Callers for the resolved qualified neighbors.
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

// CallEdges returns the `calls` graph as qualified-id → qualified-id pairs, resolving
// each edge's bare callee name to the matching qualified node(s). It is the resolved
// counterpart to Edges(ctx, "calls") (whose to_id is a raw bare name), and is what
// whole-graph structural algorithms (PageRank in repomap) consume so their node ids
// line up with Nodes(). An edge whose callee name matches no node is dropped (it names
// a symbol outside the indexed set — a stdlib/3rd-party call — and would only add a
// dangling endpoint PageRank must discard anyway); an ambiguous callee fans out to all
// definitions (the documented over-approximation). Results are unique and sorted.
func (g *Graph) CallEdges(ctx context.Context) ([]Edge, error) {
	raw, err := g.Edges(ctx, "calls")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Edge
	for _, e := range raw {
		// The from_id is already a qualified node id (BuildFile) or a literal seeded id.
		toIDs, err := g.resolveResolvable(ctx, e.To)
		if err != nil {
			return nil, err
		}
		for _, to := range toIDs {
			key := e.From + idSep + to
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Edge{From: e.From, To: to, Kind: "calls"})
		}
	}
	return out, nil
}

// resolveResolvable is resolveName restricted to endpoints that correspond to a known
// node: a bare name with no matching node returns EMPTY (the endpoint is outside the
// indexed set — a stdlib/library call), rather than echoing the bare name back. Used
// by CallEdges so PageRank never sees a phantom out-of-graph endpoint.
func (g *Graph) resolveResolvable(ctx context.Context, arg string) ([]string, error) {
	var exists int
	if err := g.db.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ? LIMIT 1`, arg).Scan(&exists); err == nil {
		return []string{arg}, nil
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	rows, err := g.db.QueryContext(ctx, `SELECT id FROM nodes WHERE name = ? ORDER BY id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// rawNeighbors runs a single-column, single-arg neighbor query and returns the raw
// stored endpoints (no resolution).
func (g *Graph) rawNeighbors(ctx context.Context, q, arg string) ([]string, error) {
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

// sortedKeys returns the map's keys, sorted ascending — the deterministic order the
// query APIs promise.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
