package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nilcore/internal/codeintel/ast"
	"nilcore/internal/codeintel/graph"
	"nilcore/internal/codeintel/lsp"
	"nilcore/internal/codeintel/retrieve"
	"nilcore/internal/codeintel/semantic"
	"nilcore/internal/embed"
)

// CodeintelTool is a READ-ONLY code-intelligence adapter over internal/codeintel
// (CV-T04). Given a query/symbol it builds a structurally-coherent context bundle
// over the worktree — semantic/lexical entry points expanded by their call-graph
// neighborhood and oriented by a few PageRank hubs — and renders it as text the
// model can read to orient in an unfamiliar repo.
//
// It is SAFE by construction and carries NO write surface:
//
//   - Host-side and read-only: it walks the worktree for source files (every
//     language ast.SupportedExtensions covers) and parses each (via codeintel/ast)
//     into an EPHEMERAL in-memory graph
//     (graph.Open(":memory:")). It never writes a file, never mutates the tree,
//     and never persists anything — the graph dies with the call.
//   - No execution: nothing here runs a model-emitted command. Parsing is pure
//     stdlib AST work; there is no shell, no sandbox escape, no `run`.
//   - No egress: it touches only files under the worktree (paths are confined by
//     the same walk discipline as SearchTool; .git is skipped). No network at all.
//
// Because the tool is non-mutating it is appropriate for the read-only roles
// (understander, and optionally the supervisor's ReadTools): NewWorker's
// write-free guarantee is preserved — "codeintel" is neither write/edit nor git.
type CodeintelTool struct{}

func (CodeintelTool) Name() string { return "codeintel" }
func (CodeintelTool) Description() string {
	return "Read-only code intelligence: given a query or symbol, return a structurally-coherent " +
		"context bundle over the repository (entry points, call-graph neighborhood, and central hubs), " +
		"each item annotated with why it was included. No writes, no execution, no network."
}
func (CodeintelTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"budget":{"type":"integer"}},"required":["query"]}`)
}

// defaultBundleBudget bounds the context bundle when the caller does not specify
// one — enough to orient without flooding the model's context.
const defaultBundleBudget = 20

// maxIndexedFiles caps how many source files the tool parses for a single call, so a
// pathologically large tree can never turn one query into an unbounded parse. The
// walk is deterministic (sorted) so the cap selects a stable prefix.
const maxIndexedFiles = 2000

func (CodeintelTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Query  string `json:"query"`
		Budget int    `json:"budget"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	budget := in.Budget
	if budget <= 0 {
		budget = defaultBundleBudget
	}

	// EPHEMERAL in-memory graph — opened, populated, and closed within this call.
	// Nothing is persisted; the tool leaves no trace on disk (read-only guarantee).
	g, err := graph.Open(":memory:")
	if err != nil {
		return "", fmt.Errorf("codeintel: open graph: %w", err)
	}
	defer g.Close()

	files, err := sourceFilesUnder(workdir)
	if err != nil {
		return "", fmt.Errorf("codeintel: scan worktree: %w", err)
	}
	if len(files) > maxIndexedFiles {
		files = files[:maxIndexedFiles]
	}
	indexed := 0
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Best-effort indexing: a file that does not parse (a fragment, a non-Go
		// payload with a .go name, generated junk) is skipped, never fatal — the
		// bundle is built over whatever parses cleanly. We pass the absolute path
		// (BuildFile parses it and records it as the node's File); renderBundle
		// makes those paths worktree-relative so the report leaks no host path.
		if berr := g.BuildFile(ctx, path); berr != nil {
			continue
		}
		indexed++
	}

	r := &retrieve.Retriever{Graph: g} // Semantic nil → lexical entry-point fallback

	// Optional semantic lens (D2-T03), opt-in via NILCORE_EMBED_KEY: a persistent,
	// content-hash-cached embedding index over the worktree files, so retrieval ranks
	// by meaning, not just lexical overlap. Absent the key, Semantic stays nil and
	// retrieval uses the graph/lexical lenses (byte-identical). The index is persistent
	// + cached (D2-T01), so only changed files re-embed across runs; the embedding key
	// rides a per-request header (I3) and the host-side index never reaches the model.
	if key := os.Getenv("NILCORE_EMBED_KEY"); key != "" {
		if sem := openSemantic(ctx, workdir, files, key); sem != nil {
			r.Semantic = sem
			defer sem.Close()
		}
	}

	// Optional precise lens: an operator-configured language server (e.g. gopls)
	// adds compiler-grade symbol matches. Opt-in via NILCORE_LSP_COMMAND (server +
	// args, space-separated); the binary is operator-trusted, never model-emitted.
	// Absent or unavailable, retrieval degrades silently to the graph-native lenses.
	if cmd := strings.Fields(os.Getenv("NILCORE_LSP_COMMAND")); len(cmd) > 0 {
		if client, stop, lerr := lsp.Spawn(ctx, cmd, "file://"+workdir); lerr == nil {
			r.LSP = client
			defer stop()
		}
	}

	bundle, err := r.Retrieve(ctx, in.Query, budget)
	if err != nil {
		return "", fmt.Errorf("codeintel: retrieve: %w", err)
	}
	return renderBundle(workdir, in.Query, indexed, bundle), nil
}

// sourceFilesUnder returns the source files under root in deterministic order, for
// every language the AST layer supports (ast.SupportedExtensions — Go and Python
// today, D3-T02), skipping .git (and any vendored/hidden VCS dir) so the index is
// reproducible. It mirrors SearchTool's walk discipline: only files under the
// worktree are ever touched, and an unreadable subtree is reported as a walk error,
// not silently half-indexed.
func sourceFilesUnder(root string) ([]string, error) {
	supported := map[string]bool{}
	for _, e := range ast.SupportedExtensions() {
		supported[strings.ToLower(e)] = true
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if supported[strings.ToLower(filepath.Ext(d.Name()))] {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// maxEmbedBytes caps the size of a single file fed to the semantic embedder, so a
// huge generated file cannot blow the embedding request.
const maxEmbedBytes = 100 * 1024

// openSemantic opens a persistent, content-hash-cached semantic index for the
// worktree (D2-T03) and indexes the given files. It is called only when
// NILCORE_EMBED_KEY is set. The index lives under the user cache dir keyed by the
// worktree path, so across runs only changed files re-embed (the D2-T01 cache).
// ANY failure (no cache dir, open error) returns nil so retrieval degrades to the
// graph/lexical lenses — semantic search is never required. Per-file Add errors
// are tolerated (best-effort indexing, like the graph build above).
func openSemantic(ctx context.Context, workdir string, files []string, key string) *semantic.Index {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(workdir))
	dbPath := filepath.Join(cache, "nilcore", "semantic", hex.EncodeToString(sum[:8])+".db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil
	}
	ix, err := semantic.Open(dbPath, embed.NewOpenAI(key, os.Getenv("NILCORE_EMBED_MODEL")))
	if err != nil {
		return nil
	}
	for _, path := range files {
		if ctx.Err() != nil {
			break
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil || len(b) == 0 {
			continue
		}
		// Index per-SYMBOL, keyed by symbol NAME — NOT per-file keyed by path. The
		// retrieval graph keys its nodes by symbol name (graph.BuildFile), and the
		// fusion step resolves a semantic hit via fileOf[hit.ID] and expands it through
		// Graph.Callees/Callers(hit.ID). A file-path key never matches a symbol-name
		// node, so a path-keyed index made the semantic lens silently dead (it resolved
		// to no file and got no graph-neighbour expansion). Keying by symbol name puts
		// the index in the graph's id space and matches the docs ("embeddings over whole
		// symbols"). Add is content-hash cached (D2-T01): an unchanged symbol body does
		// not re-embed.
		syms, serr := ast.Symbols(path)
		if serr != nil || len(syms) == 0 {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for _, s := range syms {
			if s.Name == "" {
				continue
			}
			body := symbolBody(lines, s.Span.StartLine, s.Span.EndLine)
			if body == "" || len(body) > maxEmbedBytes {
				continue
			}
			_ = ix.Add(ctx, s.Name, body)
		}
	}
	return ix
}

// symbolBody slices a symbol's source body from a file's lines using its 1-based
// inclusive line span, clamped to the file. Returns "" when the span is unusable.
func symbolBody(lines []string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}

// renderBundle turns a retrieve.Bundle into a compact, human/model-readable
// report: a header line (query + how many files were indexed) and one line per
// item carrying its symbol, file, provenance lens, and the rationale for why it
// was selected. The output is plain data; the native loop fences it as untrusted
// before it ever reaches the model's context (I7).
func renderBundle(workdir, query string, indexed int, b retrieve.Bundle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "code-intelligence bundle for %q (indexed %d source file(s))\n", query, indexed)
	if len(b.Items) == 0 {
		sb.WriteString("no relevant symbols found — try a different query or a known symbol name.")
		return sb.String()
	}
	for _, it := range b.Items {
		file := relOrSame(workdir, it.File)
		fmt.Fprintf(&sb, "- %s (%s) [%s] — %s\n", it.Symbol, file, it.Provenance, it.Rationale)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// relOrSame renders a file path relative to the worktree when possible, so the
// report never leaks an absolute host path; it returns the input unchanged if it
// is already relative or cannot be made relative.
func relOrSame(workdir, file string) string {
	if file == "" {
		return "?"
	}
	if !filepath.IsAbs(file) {
		return file
	}
	if rel, err := filepath.Rel(workdir, file); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return file
}
