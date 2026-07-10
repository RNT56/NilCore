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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// errDeliveryFailed marks a round-trip that failed BEFORE the request could reach the
// server (the send side) — e.g. writing to a dead stdio pipe. Only such a failure is
// safe for the Manager to retry on a reconnected connection: the server never received
// the call, so re-sending it cannot repeat a side effect. A failure on the RESPONSE
// side (decode/EOF/non-2xx/JSON-RPC error) is NOT wrapped — the server may already have
// executed the call, so it must never be auto-retried.
var errDeliveryFailed = errors.New("mcp: request not delivered")

// errResponseTooLarge marks a reply the peer sent that exceeds maxResponseBytes. It is a
// RESPONSE-side failure (the server received and answered the call), so it is deliberately
// NOT wrapped in errDeliveryFailed — a size-capped reply must never be auto-retried, and it
// is surfaced as a hard error rather than a silently-truncated "successful" decode.
var errResponseTooLarge = errors.New("mcp: response exceeds size cap")

// maxResponseBytes bounds a single MCP response read off the wire — the stdio JSON value,
// the HTTP body, and the accumulated SSE data of one event. MCP servers are UNTRUSTED (I7):
// without a cap a hostile or buggy server can stream an unbounded reply and exhaust host
// memory (OOM). Tool results are clipped to ~48KB downstream, so a few MiB is far more than
// any legitimate result needs while still fencing a memory-exhaustion attack. It matches the
// per-line SSE scanner cap below so no single allowed line can already blow the total.
const maxResponseBytes = 8 << 20 // 8 MiB

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
// maxMCPResponseBytes caps a single MCP response so a hostile or buggy server
// cannot exhaust host memory with one enormous reply. MCP servers are operator-
// configured but typically third-party (npx/uvx) packages, and their output is
// untrusted data (I7) — so the bound is real, not paranoia. Mirrors the provider
// layer's 8 MiB read cap (internal/provider). The stdio decoder is shared across
// messages, so it needs a per-message reset (boundedReader) rather than one
// io.LimitReader over the whole stream.
const maxMCPResponseBytes = 8 << 20

// boundedReader caps how many bytes a single decode may consume from the wrapped
// reader; reset() refreshes the per-message budget before each frame is decoded.
type boundedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (b *boundedReader) reset() { b.n = 0 }

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n >= b.max {
		return 0, fmt.Errorf("mcp response exceeded %d-byte limit", b.max)
	}
	if int64(len(p)) > b.max-b.n {
		p = p[:b.max-b.n]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	return n, err
}

type stdioTransport struct {
	mu     sync.Mutex
	enc    *json.Encoder
	dec    *json.Decoder
	rd     *capReader     // bounds bytes per decoded response, measured from the read offset (I7 OOM guard)
	lr     *boundedReader // second, per-message byte budget layered over rd (I7 OOM guard)
	closer io.Closer
	// closed is set the moment this connection is torn down (on a ctx-cancelled round-trip,
	// or by the Manager). Once set, no further round-trip touches the shared enc/dec — that
	// would race the goroutine still unwinding out of the blocking Encode/Decode we abandoned
	// — so a subsequent call short-circuits to a retryable delivery failure and the Manager
	// reconnects on a fresh transport. atomic because Close may be called concurrently (the
	// Manager tearing down) with a round-trip reading the flag.
	closed atomic.Bool
}

func newStdioTransport(rw io.ReadWriteCloser) *stdioTransport {
	// The decoder reads through two layered per-message OOM bounds so one hostile response can't
	// grow json.Decoder's buffer without limit (I7): capReader wraps the pipe and caps bytes
	// measured from the running read offset, and boundedReader wraps capReader with a fresh
	// per-message budget the decoder reads through. The encoder writes straight to rw (writes are
	// not the OOM vector).
	rd := &capReader{r: rw, cap: maxResponseBytes}
	lr := &boundedReader{r: rd, max: maxMCPResponseBytes}
	return &stdioTransport{enc: json.NewEncoder(rw), dec: json.NewDecoder(lr), rd: rd, lr: lr, closer: rw}
}

func (t *stdioTransport) roundTrip(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	if err := ctx.Err(); err != nil {
		return rpcResponse{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed.Load() {
		// A prior round-trip already tore this connection down on cancel; the abandoned
		// Encode/Decode goroutine may still be unwinding, so never touch enc/dec again.
		// Report a retryable delivery failure so the Manager evicts + reconnects fresh.
		return rpcResponse{}, fmt.Errorf("%w: send %s: transport closed", errDeliveryFailed, req.Method)
	}
	// WRITE side. t.enc.Encode is a BLOCKING pipe write with no ctx awareness: a server that
	// stops draining its stdin (e.g. it is itself blocked writing a flood of notifications to
	// a stdout nothing reads between round-trips) makes Encode block forever while holding
	// t.mu — wedging every other caller, with ctx unable to unblock it. So the send runs in a
	// goroutine and we select on ctx.Done() exactly like the read side. On cancel we CLOSE the
	// connection (which unblocks the stuck Encode and forces a reconnect) and return ctx.Err().
	// A partially-written frame may already have reached the server, so a cancel is NOT
	// errDeliveryFailed (never auto-retried); only a CLEAN Encode error — the write provably
	// never left the pipe — stays retryable. The chan is buffered so the goroutine never leaks
	// even after we return on ctx.Done().
	sendErr := make(chan error, 1)
	go func() { sendErr <- t.enc.Encode(req) }()
	select {
	case <-ctx.Done():
		_ = t.Close()
		return rpcResponse{}, ctx.Err()
	case err := <-sendErr:
		if err != nil {
			return rpcResponse{}, fmt.Errorf("%w: send %s: %v", errDeliveryFailed, req.Method, err)
		}
	}
	// READ side. A json.Decoder read over the subprocess pipe is likewise a BLOCKING call with
	// no ctx awareness, so each decode runs in a goroutine and we select on ctx.Done(). Without
	// this, a server that accepts the request but never replies (or never answers `initialize`)
	// would wedge this call — and, because roundTrip holds t.mu, EVERY other caller of this
	// shared per-server connection — indefinitely (a boot / cross-session deadlock). On
	// cancellation we CLOSE the connection: the read goroutine's Decode then returns over the
	// torn-down pipe, and the next call short-circuits errDeliveryFailed, so the Manager evicts
	// + reconnects (self-heal). The ch is buffered so the goroutine never leaks even after we
	// have already returned on ctx.Done().
	type frame struct {
		resp rpcResponse
		err  error
	}
	for {
		if err := ctx.Err(); err != nil {
			return rpcResponse{}, err
		}
		t.rd.arm()   // fresh per-response byte budget before each blocking decode (I7 OOM guard)
		t.lr.reset() // and refresh the layered per-message budget so one giant reply can't OOM the host
		ch := make(chan frame, 1)
		go func() {
			var resp rpcResponse
			err := t.dec.Decode(&resp)
			ch <- frame{resp, err}
		}()
		select {
		case <-ctx.Done():
			_ = t.Close() // tear down the poisoned reader; Manager reconnects on the next call
			return rpcResponse{}, ctx.Err()
		case f := <-ch:
			if f.err != nil {
				return rpcResponse{}, fmt.Errorf("mcp recv %s: %w", req.Method, f.err)
			}
			if f.resp.ID != req.ID {
				continue // a notification or an unrelated message — read the next frame
			}
			return f.resp, nil
		}
	}
}

// notify fires a one-way JSON-RPC notification. Like roundTrip's write side, the Encode is a
// blocking pipe write, so it honors ctx: on cancel the connection is closed (unblocking the
// stuck write) and ctx.Err() is returned, rather than wedging forever under t.mu.
func (t *stdioTransport) notify(ctx context.Context, n rpcNotification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed.Load() {
		return fmt.Errorf("%w: notify %s: transport closed", errDeliveryFailed, n.Method)
	}
	sendErr := make(chan error, 1)
	go func() { sendErr <- t.enc.Encode(n) }()
	select {
	case <-ctx.Done():
		_ = t.Close()
		return ctx.Err()
	case err := <-sendErr:
		return err
	}
}

func (t *stdioTransport) Close() error {
	t.closed.Store(true) // poison further round-trips; the abandoned goroutine keeps enc/dec to itself
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

// capReader bounds how many bytes a SINGLE decoded response may pull from the wire, so an
// untrusted MCP server cannot make json.Decoder grow its buffer without limit and exhaust
// host memory (I7). arm() is called before each Decode to grant that response a fresh budget
// of cap bytes measured from the current read offset — which correctly accounts for bytes the
// decoder buffered ahead while completing the previous message. Read trims each request to the
// remaining budget and returns errResponseTooLarge the moment the budget is spent, turning an
// oversized reply into a hard error rather than a silent truncation. It is only ever touched
// by the single decode goroutine active under t.mu, so it needs no locking of its own.
type capReader struct {
	r        io.Reader
	cap      int64
	n        int64 // total bytes read from r so far
	deadline int64 // n may not exceed this for the current response
}

func (c *capReader) arm() { c.deadline = c.n + c.cap }

func (c *capReader) Read(p []byte) (int, error) {
	if c.n >= c.deadline {
		return 0, errResponseTooLarge
	}
	if room := c.deadline - c.n; int64(len(p)) > room {
		p = p[:room] // never pull more than the remaining budget, so we trip exactly at the cap
	}
	m, err := c.r.Read(p)
	c.n += int64(m)
	return m, err
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
	proto     string // negotiated protocol version, echoed as MCP-Protocol-Version
}

func newHTTPTransport(url string, headers map[string]string, client *http.Client) *httpTransport {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpTransport{url: url, client: client, headers: headers}
}

// setProtocolVersion records the version negotiated on initialize; subsequent requests
// carry it as MCP-Protocol-Version (required by the Streamable HTTP spec, 2025-03-26+).
func (t *httpTransport) setProtocolVersion(v string) {
	t.mu.Lock()
	t.proto = v
	t.mu.Unlock()
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
	sid, proto := t.sessionID, t.proto
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if proto != "" {
		req.Header.Set("MCP-Protocol-Version", proto)
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
	// Bound the body: an untrusted server must not be able to stream an unbounded JSON reply
	// and OOM the host (I7). Read at most cap+1 bytes; if the decoder consumed all of them the
	// body overflowed the cap, which is a hard error (never a truncated-but-"successful" decode).
	lr := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	var out rpcResponse
	// Bound the response so a hostile/buggy server can't OOM the host (I7 — server output is
	// untrusted). The body is already wrapped in an io.LimitedReader above; a reply that exhausts
	// the cap is surfaced as a hard errResponseTooLarge rather than a truncated-but-"successful"
	// decode.
	if err := json.NewDecoder(lr).Decode(&out); err != nil {
		if lr.N <= 0 {
			return rpcResponse{}, fmt.Errorf("mcp http %s: %w", req.Method, errResponseTooLarge)
		}
		return rpcResponse{}, fmt.Errorf("mcp http %s decode: %w", req.Method, err)
	}
	if lr.N <= 0 {
		return rpcResponse{}, fmt.Errorf("mcp http %s: %w", req.Method, errResponseTooLarge)
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
	// A notification expects no body, but a non-2xx still signals rejection (auth, bad
	// session) — surface it so a failed handshake notify doesn't read as success.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp http notify %s: status %d", n.Method, resp.StatusCode)
	}
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
	var lastErr error // last decode failure, surfaced if no matching response is found
	flush := func() (rpcResponse, bool) {
		if data.Len() == 0 {
			return rpcResponse{}, false
		}
		raw := data.String()
		data.Reset()
		var resp rpcResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			lastErr = err               // remember it: a malformed payload is not silently lost
			return rpcResponse{}, false // a non-response / malformed SSE event — skip
		}
		if resp.ID != wantID {
			return rpcResponse{}, false
		}
		return resp, true
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // event boundary
			if resp, ok := flush(); ok {
				return resp, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			// Per the SSE spec, multiple data: lines in one event join with '\n'. The scanner
			// caps ONE line at 8MiB, but data accumulates across lines with no bound of its
			// own, so an untrusted server could OOM the host with an event of endless data:
			// lines (I7). Cap the total accumulated payload; exceeding it is a hard error.
			if data.Len() > maxResponseBytes {
				return rpcResponse{}, fmt.Errorf("mcp sse %s: %w", method, errResponseTooLarge)
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			// Bound accumulation across many data: lines in one event (the scanner
			// already caps a single line) so a hostile server can't OOM the host.
			if data.Len() > maxMCPResponseBytes {
				return rpcResponse{}, fmt.Errorf("mcp sse %s: response exceeded %d-byte limit", method, maxMCPResponseBytes)
			}
		}
		// `event:`/`id:`/comment lines are ignored — only the JSON-RPC payload matters.
	}
	if err := sc.Err(); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp sse %s: %w", method, err)
	}
	// Flush a trailing event with no final blank line.
	if resp, ok := flush(); ok {
		return resp, nil
	}
	if lastErr != nil {
		return rpcResponse{}, fmt.Errorf("mcp sse %s: malformed response: %w", method, lastErr)
	}
	return rpcResponse{}, fmt.Errorf("mcp sse %s: stream ended without a response", method)
}
