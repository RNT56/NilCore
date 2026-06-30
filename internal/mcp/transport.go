package mcp

// transport.go abstracts the JSON-RPC 2.0 wire under the MCP client so the same
// Client speaks to a server over EITHER of MCP's two transports:
//
//   - stdio: a local subprocess whose stdin/stdout carry newline-framed JSON-RPC
//     (the default for operator-launched servers, e.g. `npx …` / `uvx …`).
//   - Streamable HTTP: a remote server reached by POSTing each JSON-RPC request to
//     one URL; the reply is either a single JSON object or an SSE stream of
//     messages. The server may hand back an Mcp-Session-Id on initialize which we
//     echo on every later request.
//
// Both are stdlib-only (I6). A transport serializes its own concurrency where the
// wire requires it (stdio is a single shared reader; HTTP is naturally concurrent),
// so the Client and the Manager can call it without extra locking.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// rpcRequest / rpcNotification / rpcResponse are the JSON-RPC 2.0 frames. A request
// carries an id and expects a response; a notification has no id and no reply.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// transport is the wire seam: send a request and read its matching response, or
// fire a notification. Close releases the underlying resource (the subprocess pipe
// or the HTTP idle connections).
type transport interface {
	roundTrip(ctx context.Context, req rpcRequest) (rpcResponse, error)
	notify(ctx context.Context, n rpcNotification) error
	Close() error
}

// --- stdio transport ---------------------------------------------------------

// stdioTransport speaks newline-framed JSON-RPC over a subprocess's duplex stream.
// One shared reader means every round trip is serialized under mu, so two concurrent
// callers can never interleave reads of the same stream (a real hazard under the
// Manager's per-server reuse).
type stdioTransport struct {
	mu     sync.Mutex
	enc    *json.Encoder
	dec    *json.Decoder
	closer io.Closer
}

func newStdioTransport(rw io.ReadWriteCloser) *stdioTransport {
	return &stdioTransport{enc: json.NewEncoder(rw), dec: json.NewDecoder(rw), closer: rw}
}

func (t *stdioTransport) roundTrip(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	if err := ctx.Err(); err != nil {
		return rpcResponse{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enc.Encode(req); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp send %s: %w", req.Method, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return rpcResponse{}, err
		}
		var resp rpcResponse
		if err := t.dec.Decode(&resp); err != nil {
			return rpcResponse{}, fmt.Errorf("mcp recv %s: %w", req.Method, err)
		}
		if resp.ID != req.ID {
			continue // a notification or an unrelated message — skip it
		}
		return resp, nil
	}
}

func (t *stdioTransport) notify(_ context.Context, n rpcNotification) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enc.Encode(n)
}

func (t *stdioTransport) Close() error {
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

// --- Streamable HTTP transport ----------------------------------------------

// httpTransport speaks MCP's Streamable HTTP to a remote server: each request is a
// POST to one URL; the reply is parsed whether the server answers with a single JSON
// object or an SSE stream. An Mcp-Session-Id returned on any response is captured and
// echoed on every later request (mu-guarded; HTTP itself is concurrency-safe).
type httpTransport struct {
	url     string
	client  *http.Client
	headers map[string]string // operator-supplied static headers (e.g. Authorization)

	mu        sync.Mutex
	sessionID string
}

func newHTTPTransport(url string, headers map[string]string, client *http.Client) *httpTransport {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpTransport{url: url, client: client, headers: headers}
}

func (t *httpTransport) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return t.client.Do(req)
}

func (t *httpTransport) captureSession(resp *http.Response) {
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
}

func (t *httpTransport) roundTrip(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return rpcResponse{}, err
	}
	resp, err := t.post(ctx, body)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("mcp http %s: %w", req.Method, err)
	}
	defer resp.Body.Close()
	t.captureSession(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return rpcResponse{}, fmt.Errorf("mcp http %s: status %d: %s", req.Method, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSEResponse(resp.Body, req.ID, req.Method)
	}
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp http %s decode: %w", req.Method, err)
	}
	return out, nil
}

func (t *httpTransport) notify(ctx context.Context, n rpcNotification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	resp, err := t.post(ctx, body)
	if err != nil {
		return fmt.Errorf("mcp http notify %s: %w", n.Method, err)
	}
	defer resp.Body.Close()
	t.captureSession(resp)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

func (t *httpTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}

// readSSEResponse parses an SSE stream of JSON-RPC messages and returns the first
// one whose id matches wantID (server-initiated notifications/requests on the stream
// are skipped). The MCP server closes the stream after the response, so a clean EOF
// without a match is an error (we never block forever — ctx bounds the read).
func readSSEResponse(body io.Reader, wantID int, method string) (rpcResponse, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var data strings.Builder
	flush := func() (rpcResponse, bool, error) {
		if data.Len() == 0 {
			return rpcResponse{}, false, nil
		}
		raw := data.String()
		data.Reset()
		var resp rpcResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			return rpcResponse{}, false, nil // a non-response SSE event (e.g. a notification) — skip
		}
		if resp.ID != wantID {
			return rpcResponse{}, false, nil
		}
		return resp, true, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // event boundary
			if resp, ok, err := flush(); err != nil || ok {
				return resp, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// `event:`/`id:`/comment lines are ignored — only the JSON-RPC payload matters.
	}
	if err := sc.Err(); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp sse %s: %w", method, err)
	}
	// Flush a trailing event with no final blank line.
	if resp, ok, err := flush(); err != nil || ok {
		return resp, err
	}
	return rpcResponse{}, fmt.Errorf("mcp sse %s: stream ended without a response", method)
}
