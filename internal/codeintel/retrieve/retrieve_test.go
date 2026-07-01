package retrieve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"nilcore/internal/codeintel/graph"
	"nilcore/internal/codeintel/lsp"
	"nilcore/internal/codeintel/semantic"
)

func buildFixture(t *testing.T) *Retriever {
	t.Helper()
	ctx := context.Background()
	src := `package p
func leaf() int { return 1 }
func helper() int { return leaf() }
func Run() int { return helper() }
`
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := graph.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}

	sem, err := semantic.Open(filepath.Join(t.TempDir(), "s.db"), nil) // lexical
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sem.Close() })
	for id, text := range map[string]string{
		"Run":    "Run the program entry point",
		"helper": "helper utility",
		"leaf":   "leaf computation",
	} {
		if err := sem.Add(ctx, id, text); err != nil {
			t.Fatal(err)
		}
	}
	return &Retriever{Graph: g, Semantic: sem}
}

func TestRetrieveBundle(t *testing.T) {
	r := buildFixture(t)
	b, err := r.Retrieve(context.Background(), "Run", 10)
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]Item{}
	for _, it := range b.Items {
		byID[it.Symbol] = it
		if it.Rationale == "" || it.Provenance == "" {
			t.Errorf("item %q missing provenance/rationale: %+v", it.Symbol, it)
		}
	}
	// The lead is present...
	if _, ok := byID["Run"]; !ok {
		t.Error("bundle should include the lead Run")
	}
	// ...and its immediate neighborhood (Run calls helper) — structurally coherent.
	if it, ok := byID["helper"]; !ok || it.Provenance != "graph-neighbor" {
		t.Errorf("bundle should include helper as a graph-neighbor; got %+v", it)
	}
}

// symbolLSPServer is a minimal LSP mock over net.Pipe: it answers the handshake
// and returns one workspace/symbol match, so the precise lens can be exercised
// without a real language server.
func symbolLSPServer(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	frame := func(v any) error {
		body, _ := json.Marshal(v)
		if _, err := io.WriteString(conn, fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))); err != nil {
			return err
		}
		_, err := conn.Write(body)
		return err
	}
	for {
		n := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
				n, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if n < 0 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			_ = frame(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		case "workspace/symbol":
			_ = frame(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": []map[string]any{{
				"name":     "PreciseHit",
				"location": map[string]any{"uri": "file:///proj/x.go", "range": map[string]any{"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 1, "character": 5}}},
			}}})
		}
	}
}

// TestRetrievePreciseLens proves a wired LSP client contributes "precise" items
// (compiler-grade symbol matches), ranked ahead of the heuristic lenses.
func TestRetrievePreciseLens(t *testing.T) {
	r := buildFixture(t)
	clientConn, serverConn := net.Pipe()
	go symbolLSPServer(serverConn)
	c := lsp.NewClient(clientConn)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Initialize(context.Background(), "file:///proj"); err != nil {
		t.Fatal(err)
	}
	r.LSP = c

	b, err := r.Retrieve(context.Background(), "Run", 20)
	if err != nil {
		t.Fatal(err)
	}
	var precise *Item
	for i := range b.Items {
		if b.Items[i].Provenance == "precise" {
			precise = &b.Items[i]
		}
	}
	if precise == nil {
		t.Fatalf("bundle has no precise item:\n%+v", b.Items)
	}
	if precise.Symbol != "PreciseHit" || precise.File != "/proj/x.go" {
		t.Errorf("precise item = %+v (want PreciseHit @ /proj/x.go)", *precise)
	}
	// Precise (compiler-grade) must rank first.
	if b.Items[0].Provenance != "precise" {
		t.Errorf("precise item should rank first, got %q", b.Items[0].Provenance)
	}
}

// A nil LSP client leaves retrieval byte-identical to the graph-native lenses.
func TestRetrieveNilLSPUnchanged(t *testing.T) {
	r := buildFixture(t)
	b, err := r.Retrieve(context.Background(), "Run", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range b.Items {
		if it.Provenance == "precise" {
			t.Error("nil LSP must produce no precise items")
		}
	}
}

// TestRetrieveDropsPhantomSemanticHit proves a semantic hit whose id resolves to
// no graph node — a stale row for a renamed/deleted symbol in the persistent index
// — is dropped rather than rendered as a current "[semantic]" item. The graph is
// ground truth; the phantom must never reach the bundle.
func TestRetrieveDropsPhantomSemanticHit(t *testing.T) {
	ctx := context.Background()
	r := buildFixture(t)

	// Seed a stale row for a symbol that does NOT exist in the graph's live tree
	// (the fixture graph only has Run/helper/leaf). Its text matches the query so
	// the lexical semantic index would otherwise return it as a lead.
	if err := r.Semantic.Add(ctx, "OldName", "Run the program entry point removed"); err != nil {
		t.Fatal(err)
	}

	b, err := r.Retrieve(ctx, "Run", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range b.Items {
		if it.Symbol == "OldName" {
			t.Fatalf("phantom symbol OldName surfaced in bundle: %+v", it)
		}
	}
}

func TestRetrieveBudget(t *testing.T) {
	r := buildFixture(t)
	b, err := r.Retrieve(context.Background(), "Run", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Items) > 2 {
		t.Errorf("bundle exceeded budget: %d items", len(b.Items))
	}
}

func TestRetrieveDeterministic(t *testing.T) {
	r := buildFixture(t)
	ctx := context.Background()
	b1, _ := r.Retrieve(ctx, "Run", 10)
	b2, _ := r.Retrieve(ctx, "Run", 10)
	if len(b1.Items) != len(b2.Items) {
		t.Fatalf("nondeterministic length: %d vs %d", len(b1.Items), len(b2.Items))
	}
	for i := range b1.Items {
		if b1.Items[i].Symbol != b2.Items[i].Symbol {
			t.Errorf("nondeterministic order at %d: %q vs %q", i, b1.Items[i].Symbol, b2.Items[i].Symbol)
		}
	}
}
