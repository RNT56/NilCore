package mcp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockServer answers JSON-RPC over conn: initialize, tools/list, tools/call.
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
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"serverInfo": map[string]any{"name": "mock"}}})
		case "tools/list":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{
				{"name": "search", "description": "search docs", "inputSchema": map[string]any{"type": "object"}},
				{"name": "delete", "description": "delete a doc", "inputSchema": map[string]any{"type": "object"}},
			}}})
		case "tools/call":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": "result-ok"}}}})
		}
		// notifications/initialized has no id and no response
	}
}

func TestClientFlow(t *testing.T) {
	cConn, sConn := net.Pipe()
	go mockServer(sConn)
	c := NewClient("mock", cConn)
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
}

func TestGenerateWrappers(t *testing.T) {
	base := t.TempDir()
	tools := []Tool{
		{Name: "search", Description: "s", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fetch", Description: "f", InputSchema: json.RawMessage(`{}`)},
	}
	if err := GenerateWrappers(base, "docs", tools); err != nil {
		t.Fatalf("GenerateWrappers: %v", err)
	}
	for _, name := range []string{"search", "fetch"} {
		if _, err := os.Stat(filepath.Join(base, "mcp", "servers", "docs", name+".json")); err != nil {
			t.Fatalf("wrapper %s not written: %v", name, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(base, "mcp", "servers", "docs", "search.json"))
	if !strings.Contains(string(b), "inputSchema") || !strings.Contains(string(b), "nilcore mcp-call docs search") {
		t.Errorf("descriptor missing schema/invoke: %s", b)
	}
}
