package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStdioRoundTripHonorsCtxOnStalledRead: a stdio server that reads the request but
// never replies must NOT wedge roundTrip (which holds the per-server mutex) — the request
// ctx must unblock the blocking Decode. Regression for the boot/cross-session deadlock.
func TestStdioRoundTripHonorsCtxOnStalledRead(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer sConn.Close()
	// Server side drains the request (so Encode's pipe write completes) then never replies.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := sConn.Read(buf); err != nil {
				return
			}
		}
	}()
	st := newStdioTransport(cConn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := st.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stalled stdio read must return context.Canceled on cancel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("roundTrip did not honor ctx cancellation on a stalled read (hang)")
	}
}

// TestStdioRoundTripHonorsCtxOnBlockedWrite: the WRITE side must be ctx-cancellable too. A
// server that stops draining its stdin makes t.enc.Encode block forever while holding t.mu,
// wedging every other caller. net.Pipe is synchronous — with no reader on the server side the
// very first Encode blocks — so this reproduces the write-side deadlock. Regression for it.
func TestStdioRoundTripHonorsCtxOnBlockedWrite(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer sConn.Close() // server side NEVER reads: the client's Encode blocks with nothing draining
	defer cConn.Close()
	st := newStdioTransport(cConn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := st.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the goroutine reach the blocked Encode
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked stdio write must return context.Canceled on cancel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("roundTrip did not honor ctx cancellation on a blocked write (write-side deadlock)")
	}
}

// TestStdioCancelReapsChildViaProcessRW proves the whole cancel→teardown→reap chain: a
// ctx-cancelled round-trip closes the transport, which (through processRW.Close) reaps the
// child, so a stdio subprocess is never left a zombie until the next call or Manager.Close.
func TestStdioCancelReapsChildViaProcessRW(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer sConn.Close()
	var reaped atomic.Int32
	// processRW over the synchronous pipe: the server never drains, so Encode blocks; on
	// cancel the transport's Close calls processRW.Close, which must invoke reap.
	prw := &processRW{w: cConn, r: cConn, reap: func() { reaped.Add(1) }}
	st := newStdioTransport(prw)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = st.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled round-trip hung instead of tearing down")
	}
	if reaped.Load() == 0 {
		t.Fatal("a ctx-cancelled round-trip must reap the child (processRW.reap) — zombie leak")
	}
}

// TestCapReaderTripsAtCap unit-tests the per-response byte cap: it must stop EXACTLY at the
// cap with errResponseTooLarge (a hard error, never a silently-truncated "ok" read), and a
// fresh arm() must grant the next message its own budget (long-lived stdio reuse).
func TestCapReaderTripsAtCap(t *testing.T) {
	cr := &capReader{r: bytes.NewReader(make([]byte, 1000)), cap: 100}
	cr.arm()
	buf := make([]byte, 64)
	var total int
	for {
		n, err := cr.Read(buf)
		total += n
		if err != nil {
			if !errors.Is(err, errResponseTooLarge) {
				t.Fatalf("want errResponseTooLarge at the cap, got %v", err)
			}
			break
		}
	}
	if total != 100 {
		t.Fatalf("capReader let %d bytes through, want exactly the 100-byte cap", total)
	}
	cr.arm() // a new message gets a fresh budget
	if n, err := cr.Read(buf); err != nil || n == 0 {
		t.Fatalf("after re-arm the reader must yield the next message's bytes, got n=%d err=%v", n, err)
	}
}

// oversizedJSONResult streams a valid JSON-RPC response whose text field alone exceeds the
// response cap, so decoding it must trip errResponseTooLarge rather than buffer it all.
func oversizedJSONResult(w io.Writer) {
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"`)
	chunk := strings.Repeat("A", 64*1024)
	for written := 0; written < maxResponseBytes+len(chunk); written += len(chunk) {
		_, _ = io.WriteString(w, chunk)
	}
	_, _ = io.WriteString(w, `"}]}}`+"\n")
}

// TestStdioResponseCapRejectsOversized: a hostile stdio server streaming an unbounded reply
// must be rejected (errResponseTooLarge), not buffered into an OOM (I7).
func TestStdioResponseCapRejectsOversized(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	go func() {
		defer sConn.Close()
		var req map[string]any
		_ = json.NewDecoder(sConn).Decode(&req) // drain the request so the client's Encode completes
		oversizedJSONResult(sConn)
	}()
	st := newStdioTransport(cConn)
	_, err := st.roundTrip(context.Background(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("oversized stdio response must be rejected as errResponseTooLarge, got %v", err)
	}
}

// TestHTTPResponseCapRejectsOversized: same guard on the HTTP body path.
func TestHTTPResponseCapRejectsOversized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		oversizedJSONResult(w)
	}))
	defer srv.Close()
	ht := newHTTPTransport(srv.URL, nil, srv.Client())
	_, err := ht.roundTrip(context.Background(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("oversized HTTP body must be rejected as errResponseTooLarge, got %v", err)
	}
}

// TestSSEResponseCapRejectsOversized: the SSE data accumulator must cap total bytes across
// many data: lines in one event (each line is under the scanner cap, but the sum is not).
func TestSSEResponseCapRejectsOversized(t *testing.T) {
	var sb strings.Builder
	line := "data: " + strings.Repeat("x", 60*1024) + "\n"
	for sb.Len() < maxResponseBytes+len(line) {
		sb.WriteString(line)
	}
	_, err := readSSEResponse(strings.NewReader(sb.String()), 1, "tools/call")
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("oversized SSE data accumulation must be rejected as errResponseTooLarge, got %v", err)
	}
}

// TestStdioRoundTripBoundsResponse: a hostile or buggy MCP server that returns an
// enormous reply must NOT be read unbounded into host memory — roundTrip fails once
// the reply exceeds the per-message cap, well before the ctx deadline. Regression for
// the MCP OOM (server output is untrusted, I7).
func TestStdioRoundTripBoundsResponse(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	// Server drains the request, then streams a valid-JSON reply far larger than the
	// cap (a never-ending "result" string). net.Pipe is synchronous, so an unbounded
	// reader would keep pulling bytes forever; the boundedReader must stop it.
	go func() {
		buf := make([]byte, 4096)
		_, _ = sConn.Read(buf) // consume the request
		_, _ = sConn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"x":"`))
		chunk := []byte(strings.Repeat("A", 1<<16))
		for i := 0; i < (maxMCPResponseBytes/len(chunk))+64; i++ {
			if _, err := sConn.Write(chunk); err != nil {
				return // client tore down after hitting the cap — expected
			}
		}
		_ = sConn.Close()
	}()
	st := newStdioTransport(cConn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := st.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
	if err == nil {
		t.Fatal("roundTrip accepted an over-limit response (unbounded read)")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bound did not trip before the ctx deadline — reader was effectively unbounded: %v", err)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("want a size-limit error, got %v", err)
	}
}

// TestGenerateWrappersSanitizesTraversalToolName: an UNTRUSTED tool name (server output)
// containing path traversal must be confined — the descriptor stays inside the server dir
// and the JSON keeps the original name so the model still invokes the right tool.
func TestGenerateWrappersSanitizesTraversalToolName(t *testing.T) {
	base := t.TempDir()
	evil := "../../../../etc/evil"
	if err := GenerateWrappers(base, "docs", []Tool{
		{Name: evil, Description: "x", InputSchema: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("GenerateWrappers: %v", err)
	}
	dir := filepath.Join(base, "mcp", "servers", "docs")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("want exactly 1 descriptor in the server dir, got %d: %v", len(ents), ents)
	}
	name := ents[0].Name()
	if strings.ContainsAny(name, `/\`) || name == ".." || strings.HasPrefix(name, "../") {
		t.Fatalf("filename not sanitized against traversal: %q", name)
	}
	abs := filepath.Join(dir, name)
	if !strings.HasPrefix(abs, dir+string(filepath.Separator)) {
		t.Fatalf("descriptor %q escaped the server dir %q", abs, dir)
	}
	b, _ := os.ReadFile(abs)
	if !strings.Contains(string(b), evil) {
		t.Errorf("descriptor must keep the ORIGINAL tool name %q so the model invokes it: %s", evil, b)
	}
}

// dispatch is the shared mock MCP handler: it answers initialize, tools/list,
// tools/call, resources/*, and prompts/*. It returns (resultObject, ok); ok=false for
// a method it doesn't implement (the caller emits a JSON-RPC "method not found").
func dispatch(method string) (any, bool) {
	switch method {
	case "initialize":
		return map[string]any{"serverInfo": map[string]any{"name": "mock"}}, true
	case "tools/list":
		return map[string]any{"tools": []map[string]any{
			{"name": "search", "description": "search docs", "inputSchema": map[string]any{"type": "object"}},
			{"name": "delete", "description": "delete a doc", "inputSchema": map[string]any{"type": "object"}},
		}}, true
	case "tools/call":
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "result-ok"}}}, true
	case "resources/list":
		return map[string]any{"resources": []map[string]any{
			{"uri": "file://a.txt", "name": "A", "description": "doc A", "mimeType": "text/plain"},
		}}, true
	case "resources/read":
		return map[string]any{"contents": []map[string]any{{"type": "text", "text": "resource-body"}}}, true
	case "prompts/list":
		return map[string]any{"prompts": []map[string]any{{"name": "greet", "description": "greeting"}}}, true
	case "prompts/get":
		return map[string]any{"description": "D", "messages": []map[string]any{
			{"role": "user", "content": map[string]any{"type": "text", "text": "hello"}},
		}}, true
	}
	return nil, false
}

// mockServer answers JSON-RPC over a stdio-style conn (newline-framed).
func mockServer(conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if res, ok := dispatch(req.Method); ok {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		}
		// notifications (no id) get no response
	}
}

// httpMCPHandler answers JSON-RPC over Streamable HTTP. If sse is true it replies as an
// event stream; otherwise as a single JSON object. It echoes an Mcp-Session-Id.
func httpMCPHandler(sse bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-123")
		res, ok := dispatch(req.Method)
		if !ok { // a notification or unimplemented method
			w.WriteHeader(http.StatusAccepted)
			return
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: " + string(payload) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}
}

func TestClientFlow_Stdio(t *testing.T) {
	cConn, sConn := net.Pipe()
	go mockServer(sConn)
	c := NewClient("mock", newStdioTransport(cConn))
	defer c.Close()
	ctx := context.Background()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 2 {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	out, err := c.CallTool(ctx, "search", json.RawMessage(`{"q":"x"}`))
	if err != nil || out != "result-ok" {
		t.Fatalf("CallTool = %q, %v", out, err)
	}
	// Opt-in surfaces.
	if res, err := c.ReadResource(ctx, "file://a.txt"); err != nil || res != "resource-body" {
		t.Fatalf("ReadResource = %q, %v", res, err)
	}
	if pr, err := c.GetPrompt(ctx, "greet", nil); err != nil || !strings.Contains(pr, "hello") {
		t.Fatalf("GetPrompt = %q, %v", pr, err)
	}
}

// TestClientFlow_HTTP proves the SAME client works over the Streamable HTTP transport,
// for both JSON and SSE replies, with session-id capture.
func TestClientFlow_HTTP(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(httpMCPHandler(sse))
			defer srv.Close()
			c := NewClient("remote", newHTTPTransport(srv.URL, nil, srv.Client()))
			defer c.Close()
			ctx := context.Background()
			if err := c.Initialize(ctx); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			out, err := c.CallTool(ctx, "search", json.RawMessage(`{"q":"x"}`))
			if err != nil || out != "result-ok" {
				t.Fatalf("CallTool = %q, %v", out, err)
			}
			// The session id captured on initialize is echoed on later requests.
			if ht := c.t.(*httpTransport); ht.sessionID != "sess-123" {
				t.Errorf("session id = %q, want sess-123", ht.sessionID)
			}
		})
	}
}

// TestManagerReuseCallClose drives the Manager end-to-end over an HTTP server (testable
// without a real subprocess): connect+initialize lazily, reuse across calls, and tear
// down on Close.
func TestManagerReuseCallClose(t *testing.T) {
	srv := httptest.NewServer(httpMCPHandler(false))
	defer srv.Close()
	m := NewManager(Config{Servers: []ServerSpec{{Name: "remote", URL: srv.URL}}})
	defer m.Close()
	ctx := context.Background()

	for i := 0; i < 3; i++ { // repeated calls reuse the cached connection
		out, err := m.CallTool(ctx, "remote", "search", json.RawMessage(`{}`))
		if err != nil || out != "result-ok" {
			t.Fatalf("CallTool[%d] = %q, %v", i, out, err)
		}
	}
	if res, err := m.ReadResource(ctx, "remote", "file://a.txt"); err != nil || res != "resource-body" {
		t.Fatalf("ReadResource = %q, %v", res, err)
	}
	if _, err := m.CallTool(ctx, "nope", "x", nil); err == nil {
		t.Error("an unknown server must error")
	}
}

// TestCallToolFailureNotRetried: a tool-level isError is surfaced as ErrToolFailed (so
// the Manager won't re-run it and repeat a side effect).
func TestCallToolFailureNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "tools/call" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":{"isError":true,"content":[{"type":"text","text":"boom"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":{}}`))
	}))
	defer srv.Close()
	c := NewClient("remote", newHTTPTransport(srv.URL, nil, srv.Client()))
	defer c.Close()
	_, err := c.CallTool(context.Background(), "delete", nil)
	if !errors.Is(err, ErrToolFailed) {
		t.Fatalf("a failing tool must wrap ErrToolFailed, got %v", err)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// TestDeliveryFailedClassification pins the retry-safety contract: ONLY a send-side
// failure (request never delivered) is errDeliveryFailed and thus retryable; a server-
// received failure (HTTP non-2xx) is NOT — so the Manager never re-runs a call the
// server may already have executed.
func TestDeliveryFailedClassification(t *testing.T) {
	// stdio send to a closed pipe → delivery failure (retryable).
	cConn, _ := net.Pipe()
	_ = cConn.Close()
	st := newStdioTransport(cConn)
	_, err := st.roundTrip(context.Background(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
	if !errors.Is(err, errDeliveryFailed) {
		t.Fatalf("send to closed pipe must be errDeliveryFailed, got %v", err)
	}

	// HTTP non-2xx → server received it → NOT a delivery failure (not retryable).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ht := newHTTPTransport(srv.URL, nil, srv.Client())
	_, err = ht.roundTrip(context.Background(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"})
	if err == nil || errors.Is(err, errDeliveryFailed) {
		t.Fatalf("HTTP 500 must error but NOT as errDeliveryFailed, got %v", err)
	}
}

// TestManagerConcurrentSingleFlight: many concurrent first-calls to one server open the
// connection exactly once (no double-spawn) and all succeed. Run under -race.
func TestManagerConcurrentSingleFlight(t *testing.T) {
	var inits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			atomic.AddInt32(&inits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		res, _ := dispatch(req.Method)
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	// Route connect()'s HTTP through the httptest client.
	old := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = old }()

	m := NewManager(Config{Servers: []ServerSpec{{Name: "remote", URL: srv.URL}}})
	defer m.Close()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := m.CallTool(context.Background(), "remote", "search", json.RawMessage(`{}`)); err != nil || out != "result-ok" {
				t.Errorf("concurrent CallTool = %q, %v", out, err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&inits); n != 1 {
		t.Fatalf("server initialized %d times, want exactly 1 (single-flight)", n)
	}
}

// TestHTTPProtocolVersionHeader: after the handshake, subsequent requests carry the
// negotiated MCP-Protocol-Version (Streamable HTTP spec requirement).
func TestHTTPProtocolVersionHeader(t *testing.T) {
	var sawVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "tools/call" {
			sawVersion = r.Header.Get("MCP-Protocol-Version")
		}
		w.Header().Set("Content-Type", "application/json")
		res, _ := dispatch(req.Method)
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	c := NewClient("remote", newHTTPTransport(srv.URL, nil, srv.Client()))
	defer c.Close()
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "search", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if sawVersion == "" {
		t.Error("post-initialize request did not carry MCP-Protocol-Version")
	}
}

func TestGenerateWrappers(t *testing.T) {
	base := t.TempDir()
	if err := GenerateWrappers(base, "docs", []Tool{
		{Name: "search", Description: "s", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fetch", Description: "f", InputSchema: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("GenerateWrappers: %v", err)
	}
	for _, name := range []string{"search", "fetch"} {
		if _, err := os.Stat(filepath.Join(base, "mcp", "servers", "docs", name+".json")); err != nil {
			t.Fatalf("wrapper %s not written: %v", name, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(base, "mcp", "servers", "docs", "search.json"))
	s := string(b)
	if !strings.Contains(s, "inputSchema") || !strings.Contains(s, "search") || !strings.Contains(s, "mcp") {
		t.Errorf("descriptor missing schema/tool/mcp-invoke: %s", s)
	}
}

// TestGenerateWrappersPrunesStale: a regenerate with a smaller tool set removes the
// descriptor of a tool the server dropped (so it can't stay discoverable), while a
// resources/ subdir is left untouched.
func TestGenerateWrappersPrunesStale(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "mcp", "servers", "docs")
	if err := GenerateWrappers(base, "docs", []Tool{
		{Name: "search", Description: "s", InputSchema: json.RawMessage(`{}`)},
		{Name: "delete", Description: "d", InputSchema: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("first GenerateWrappers: %v", err)
	}
	if err := GenerateResourceWrappers(base, "docs", []Resource{{URI: "file://a.txt", Name: "A"}}); err != nil {
		t.Fatalf("GenerateResourceWrappers: %v", err)
	}
	// Re-gen with "delete" removed: its descriptor must be pruned, "search" kept.
	if err := GenerateWrappers(base, "docs", []Tool{
		{Name: "search", Description: "s", InputSchema: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("re-GenerateWrappers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "delete.json")); !os.IsNotExist(err) {
		t.Errorf("stale descriptor delete.json was not pruned (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "search.json")); err != nil {
		t.Errorf("live descriptor search.json must survive: %v", err)
	}
	// The resources/ subdir must be untouched by the tool-descriptor prune.
	if _, err := os.Stat(filepath.Join(dir, "resources")); err != nil {
		t.Errorf("resources/ subdir must not be pruned by GenerateWrappers: %v", err)
	}
}

func TestGenerateResourceAndPromptWrappers(t *testing.T) {
	base := t.TempDir()
	if err := GenerateResourceWrappers(base, "docs", []Resource{{URI: "file://a.txt", Name: "A"}}); err != nil {
		t.Fatalf("GenerateResourceWrappers: %v", err)
	}
	if err := GeneratePromptWrappers(base, "docs", []Prompt{{Name: "greet", Description: "g"}}); err != nil {
		t.Fatalf("GeneratePromptWrappers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "mcp", "servers", "docs", "resources", "A.json")); err != nil {
		t.Errorf("resource descriptor not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "mcp", "servers", "docs", "prompts", "greet.json")); err != nil {
		t.Errorf("prompt descriptor not written: %v", err)
	}
}

// TestGenerateResourceWrappersPrunesStale: like the tool path, regenerating the resource set
// must prune a descriptor for a resource the server dropped (so it can't stay discoverable),
// and an EMPTY regeneration must clear them all.
func TestGenerateResourceWrappersPrunesStale(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "mcp", "servers", "docs", "resources")
	if err := GenerateResourceWrappers(base, "docs", []Resource{
		{URI: "file://a.txt", Name: "A"},
		{URI: "file://b.txt", Name: "B"},
	}); err != nil {
		t.Fatalf("GenerateResourceWrappers: %v", err)
	}
	// Regen with B removed → B pruned, A kept.
	if err := GenerateResourceWrappers(base, "docs", []Resource{{URI: "file://a.txt", Name: "A"}}); err != nil {
		t.Fatalf("re-GenerateResourceWrappers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "A.json")); err != nil {
		t.Errorf("live resource A.json must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "B.json")); !os.IsNotExist(err) {
		t.Errorf("stale resource B.json must be pruned (err=%v)", err)
	}
	// Empty regen → everything pruned.
	if err := GenerateResourceWrappers(base, "docs", nil); err != nil {
		t.Fatalf("empty GenerateResourceWrappers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "A.json")); !os.IsNotExist(err) {
		t.Errorf("empty regen must prune all resources (A.json err=%v)", err)
	}
}

// TestGeneratePromptWrappersPrunesStale: same reconcile for prompts.
func TestGeneratePromptWrappersPrunesStale(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "mcp", "servers", "docs", "prompts")
	if err := GeneratePromptWrappers(base, "docs", []Prompt{{Name: "greet"}, {Name: "bye"}}); err != nil {
		t.Fatalf("GeneratePromptWrappers: %v", err)
	}
	if err := GeneratePromptWrappers(base, "docs", []Prompt{{Name: "greet"}}); err != nil {
		t.Fatalf("re-GeneratePromptWrappers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "greet.json")); err != nil {
		t.Errorf("live prompt greet.json must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bye.json")); !os.IsNotExist(err) {
		t.Errorf("stale prompt bye.json must be pruned (err=%v)", err)
	}
}

// TestGenerateWrappersEmptyOnMissingDirIsNoop: pruning an empty set when no descriptors were
// ever generated must be a clean no-op, not a read-error on the missing dir.
func TestGenerateWrappersEmptyOnMissingDirIsNoop(t *testing.T) {
	base := t.TempDir()
	if err := GenerateResourceWrappers(base, "docs", nil); err != nil {
		t.Errorf("empty resources on a fresh base must be a no-op, got %v", err)
	}
	if err := GeneratePromptWrappers(base, "docs", nil); err != nil {
		t.Errorf("empty prompts on a fresh base must be a no-op, got %v", err)
	}
}
