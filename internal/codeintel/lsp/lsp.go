// Package lsp is a minimal Language Server Protocol client (P3-T12), SCIP-aligned
// in spirit: where the ast/graph packages give us a Go-native structural view,
// an LSP server gives us cross-language, compiler-grade definitions and
// references. We speak just enough of the protocol to ask "where is this symbol
// defined?" and "who references it?" — the two queries that anchor navigation.
//
// The wire format is JSON-RPC 2.0 over a byte stream with Content-Length
// framing: each message is "Content-Length: N\r\n\r\n" followed by N bytes of
// JSON. This file owns the framing on both read and write so the rest of the
// codebase never touches the transport. Reads skip notifications and any
// response whose id does not match the request we are waiting on, so an
// unsolicited server message never gets mistaken for our answer.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Location is a resolved source position, mapped from an LSP Location/Range.
// Lines and characters are 0-based, matching the protocol.
type Location struct {
	URI       string
	StartLine int
	StartChar int
	EndLine   int
	EndChar   int
}

// Client is a minimal LSP client over an io.ReadWriteCloser transport
// (typically a server subprocess's stdio). It is safe for sequential use; the
// id counter is guarded so concurrent callers still get distinct request ids.
type Client struct {
	rw  io.ReadWriteCloser
	r   *bufio.Reader
	mu  sync.Mutex // serializes a request/response round-trip on the shared stream
	idc int64
}

// NewClient wraps a transport. The caller owns opening the transport; Close
// closes it.
func NewClient(rw io.ReadWriteCloser) *Client {
	return &Client{rw: rw, r: bufio.NewReader(rw)}
}

// Close closes the underlying transport.
func (c *Client) Close() error { return c.rw.Close() }

// --- JSON-RPC envelopes ---

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// --- LSP param/result shapes (only the fields we use) ---

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

func (l lspLocation) toLocation() Location {
	return Location{
		URI:       l.URI,
		StartLine: l.Range.Start.Line,
		StartChar: l.Range.Start.Character,
		EndLine:   l.Range.End.Line,
		EndChar:   l.Range.End.Character,
	}
}

// Initialize performs the LSP handshake: the "initialize" request followed by
// the "initialized" notification. rootURI is the workspace root (a file:// URI).
func (c *Client) Initialize(ctx context.Context, rootURI string) error {
	params := map[string]any{
		"processId":    nil,
		"rootUri":      rootURI,
		"capabilities": map[string]any{},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notify: %w", err)
	}
	return nil
}

// Definition resolves textDocument/definition at the given position. The server
// may answer with a single Location object or an array of them; both are handled.
func (c *Client) Definition(ctx context.Context, uri string, line, char int) ([]Location, error) {
	raw, err := c.call(ctx, "textDocument/definition", textDocumentPosition(uri, line, char))
	if err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}
	return parseLocations(raw)
}

// References resolves textDocument/references at the given position. The
// declaration itself is included (includeDeclaration: true).
func (c *Client) References(ctx context.Context, uri string, line, char int) ([]Location, error) {
	params := textDocumentPosition(uri, line, char)
	params["context"] = map[string]any{"includeDeclaration": true}
	raw, err := c.call(ctx, "textDocument/references", params)
	if err != nil {
		return nil, fmt.Errorf("references: %w", err)
	}
	return parseLocations(raw)
}

func textDocumentPosition(uri string, line, char int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
}

// parseLocations accepts a JSON-RPC result that is null, a single Location
// object, or an array of Locations, and normalizes to []Location.
func parseLocations(raw json.RawMessage) ([]Location, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []lspLocation
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("decode locations array: %w", err)
		}
		out := make([]Location, 0, len(arr))
		for _, l := range arr {
			out = append(out, l.toLocation())
		}
		return out, nil
	}
	var single lspLocation
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("decode location: %w", err)
	}
	return []Location{single.toLocation()}, nil
}

// --- transport: Content-Length framing ---

// call sends a request and blocks for the matching response. Notifications and
// responses with a non-matching id are skipped while waiting.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.idc++
	id := c.idc
	if err := c.writeMessage(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := c.readResponse()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", method, err)
		}
		if resp.ID == nil || *resp.ID != id {
			continue // notification or a response we are not waiting on
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// notify sends a one-way notification (no id, no response expected).
func (c *Client) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeMessage(notification{JSONRPC: "2.0", Method: method, Params: params})
}

// writeMessage marshals v and writes it with a Content-Length header.
func (c *Client) writeMessage(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(c.rw, header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := c.rw.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// readResponse reads one Content-Length-framed JSON-RPC message.
func (c *Client) readResponse() (response, error) {
	n, err := c.readContentLength()
	if err != nil {
		return response{}, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return response{}, fmt.Errorf("read body: %w", err)
	}
	var resp response
	if err := json.Unmarshal(body, &resp); err != nil {
		return response{}, fmt.Errorf("decode message: %w", err)
	}
	return resp, nil
}

// readContentLength consumes the header block and returns the byte length of the
// following JSON body. Header lines end in CRLF; an empty line ends the block.
func (c *Client) readContentLength() (int, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("read header line: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return 0, fmt.Errorf("malformed header line: %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return 0, fmt.Errorf("parse Content-Length: %w", err)
			}
		}
	}
	if length < 0 {
		return 0, fmt.Errorf("missing Content-Length header")
	}
	return length, nil
}
