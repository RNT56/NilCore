package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
