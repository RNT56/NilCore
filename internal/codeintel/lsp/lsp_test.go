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

// mockServer answers the minimal handshake plus one definition. It replies to
// "initialize" with an empty result, swallows the "initialized" notification,
// and answers "textDocument/definition" with a single Location. Reported errors
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
		case "textDocument/definition":
			loc := map[string]any{
				"uri": "file:///proj/main.go",
				"range": map[string]any{
					"start": map[string]any{"line": 41, "character": 5},
					"end":   map[string]any{"line": 41, "character": 12},
				},
			}
			if err := writeFramed(conn, map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": loc,
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

func TestInitializeAndDefinition(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	errc := make(chan error, 1)
	go mockServer(serverConn, errc)

	c := NewClient(clientConn)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	if err := c.Initialize(ctx, "file:///proj"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	locs, err := c.Definition(ctx, "file:///proj/main.go", 41, 8)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Definition returned %d locations, want 1", len(locs))
	}
	got := locs[0]
	want := Location{URI: "file:///proj/main.go", StartLine: 41, StartChar: 5, EndLine: 41, EndChar: 12}
	if got != want {
		t.Errorf("Definition location = %+v, want %+v", got, want)
	}

	select {
	case err := <-errc:
		t.Fatalf("mock server error: %v", err)
	default:
	}
}

// TestParseLocationsArray covers the array-result branch (textDocument/references
// shape) directly, since the protocol allows both single and array forms.
func TestParseLocationsArray(t *testing.T) {
	raw := json.RawMessage(`[
		{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":4}}},
		{"uri":"file:///b.go","range":{"start":{"line":9,"character":0},"end":{"line":9,"character":3}}}
	]`)
	locs, err := parseLocations(raw)
	if err != nil {
		t.Fatalf("parseLocations: %v", err)
	}
	want := []Location{
		{URI: "file:///a.go", StartLine: 1, StartChar: 2, EndLine: 1, EndChar: 4},
		{URI: "file:///b.go", StartLine: 9, StartChar: 0, EndLine: 9, EndChar: 3},
	}
	if len(locs) != len(want) {
		t.Fatalf("got %d locations, want %d", len(locs), len(want))
	}
	for i := range want {
		if locs[i] != want[i] {
			t.Errorf("location[%d] = %+v, want %+v", i, locs[i], want[i])
		}
	}
}

// TestParseLocationsNull confirms a null result yields no locations and no error.
func TestParseLocationsNull(t *testing.T) {
	locs, err := parseLocations(json.RawMessage(`null`))
	if err != nil {
		t.Fatalf("parseLocations(null): %v", err)
	}
	if locs != nil {
		t.Errorf("parseLocations(null) = %v, want nil", locs)
	}
}
