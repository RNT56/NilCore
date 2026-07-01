package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// readFramed reads one Content-Length-framed JSON-RPC message from r and
// unmarshals it into v. It mirrors the client's own framing so the test
// exercises the same wire format end to end.
func readFramed(r *bufio.Reader, v any) error {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return err
			}
		}
	}
	if length < 0 {
		return fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// writeFramed writes v as a Content-Length-framed JSON-RPC message to w.
func writeFramed(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// mockServer answers the minimal handshake plus one workspace/symbol. It replies
// to "initialize" with an empty result, swallows the "initialized" notification,
// and answers "workspace/symbol" with a single SymbolInformation. Reported errors
// land on errc; the harness fails the test if any appear.
func mockServer(conn net.Conn, errc chan<- error) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)

	for {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := readFramed(r, &req); err != nil {
			if err != io.EOF {
				errc <- err
			}
			return
		}
		switch req.Method {
		case "initialize":
			if err := writeFramed(conn, map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{},
			}); err != nil {
				errc <- err
				return
			}
		case "initialized":
			// notification: no reply
		case "workspace/symbol":
			syms := []map[string]any{{
				"name": "Run",
				"location": map[string]any{
					"uri": "file:///proj/run.go",
					"range": map[string]any{
						"start": map[string]any{"line": 10, "character": 0},
						"end":   map[string]any{"line": 10, "character": 3},
					},
				},
			}}
			if err := writeFramed(conn, map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": syms,
			}); err != nil {
				errc <- err
				return
			}
		default:
			errc <- fmt.Errorf("unexpected method %q", req.Method)
			return
		}
	}
}

// TestInitializeHandshake covers the initialize + initialized handshake against
// the mock server, the prerequisite every query depends on.
func TestInitializeHandshake(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	errc := make(chan error, 1)
	go mockServer(serverConn, errc)

	c := NewClient(clientConn)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	if err := c.Initialize(ctx, "file:///proj"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	select {
	case err := <-errc:
		t.Fatalf("mock server error: %v", err)
	default:
	}
}

// TestSymbol covers the workspace/symbol path — the precise entry point retrieval
// uses (a name query, no source position required).
func TestSymbol(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	errc := make(chan error, 1)
	go mockServer(serverConn, errc)

	c := NewClient(clientConn)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Initialize(ctx, "file:///proj"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	hits, err := c.Symbol(ctx, "Run")
	if err != nil {
		t.Fatalf("Symbol: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "Run" || hits[0].Location.URI != "file:///proj/run.go" {
		t.Fatalf("Symbol = %+v", hits)
	}
}

// TestReadContentLengthCap proves an oversized Content-Length (attacker-controlled
// per I7: a buggy/wedged server) is rejected with an error rather than triggering
// an arbitrarily large make([]byte, n). A length just under the cap is accepted.
func TestReadContentLengthCap(t *testing.T) {
	read := func(header string) (int, error) {
		c := &Client{r: bufio.NewReader(strings.NewReader(header))}
		return c.readContentLength()
	}

	// Well over the cap: must error, and must not report a usable length.
	if n, err := read(fmt.Sprintf("Content-Length: %d\r\n\r\n", int64(maxLSPMessage)+1)); err == nil {
		t.Fatalf("oversized Content-Length accepted (n=%d), want error", n)
	}

	// At the cap boundary: still accepted (the body read, not attempted here, is
	// what would fail on a short stream — the length itself is legal).
	if n, err := read(fmt.Sprintf("Content-Length: %d\r\n\r\n", maxLSPMessage)); err != nil {
		t.Fatalf("Content-Length at cap rejected: %v", err)
	} else if n != maxLSPMessage {
		t.Fatalf("Content-Length at cap = %d, want %d", n, maxLSPMessage)
	}
}

// TestSpawnMissingCommand proves Spawn degrades cleanly (clear error, no hang) for
// a missing binary or empty command, so the codeintel tool falls back gracefully.
func TestSpawnMissingCommand(t *testing.T) {
	if _, _, err := Spawn(context.Background(), []string{"nilcore-no-such-lsp-xyz"}, "file:///p"); err == nil {
		t.Error("Spawn of a missing binary must error")
	}
	if _, _, err := Spawn(context.Background(), nil, "file:///p"); err == nil {
		t.Error("Spawn with an empty command must error")
	}
}
