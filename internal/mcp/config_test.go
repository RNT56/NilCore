package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestHelperMCPServer is a stdio MCP server when run as a subprocess with
// NILCORE_MCP_MOCK=1 (the canonical Go self-exec pattern). It answers the handshake
// and echoes a fixed tools/call result, then exits on EOF so the test framework's
// own stdout never pollutes the JSON-RPC stream the parent reads.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("NILCORE_MCP_MOCK") != "1" {
		return
	}
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := dec.Decode(&req); err != nil {
			os.Exit(0)
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		case "tools/call":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echoed-ok"}},
			}})
		}
	}
}

// TestCallEndToEnd drives a real Call through Spawn → Initialize → CallTool over a
// subprocess's stdio, exercising the processRW bridge end to end.
func TestCallEndToEnd(t *testing.T) {
	t.Setenv("NILCORE_MCP_MOCK", "1") // the spawned child inherits this and acts as the server
	spec := ServerSpec{Name: "mock", Command: []string{os.Args[0], "-test.run=TestHelperMCPServer"}}
	out, err := Call(context.Background(), spec, "search", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "echoed-ok" {
		t.Errorf("Call result = %q, want echoed-ok", out)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	js := `{"servers":[{"name":"docs","command":["docs-server","--stdio"]},{"name":"db","command":["dbmcp"]}]}`
	if err := os.WriteFile(path, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("loaded %d servers, want 2", len(cfg.Servers))
	}
	spec, ok := cfg.Server("docs")
	if !ok || len(spec.Command) != 2 || spec.Command[0] != "docs-server" {
		t.Errorf("docs spec = %+v, ok=%v", spec, ok)
	}
	if _, ok := cfg.Server("missing"); ok {
		t.Error("unknown server must not resolve")
	}

	// A missing config file is not an error — no servers configured.
	none, err := LoadConfig(filepath.Join(dir, "nope.json"))
	if err != nil || len(none.Servers) != 0 {
		t.Errorf("absent config = %+v, %v", none, err)
	}
}

func TestSpawnGracefulFailure(t *testing.T) {
	if _, _, err := Spawn(context.Background(), ServerSpec{Name: "x"}); err == nil {
		t.Error("empty command must error")
	}
	if _, _, err := Spawn(context.Background(), ServerSpec{Name: "x", Command: []string{"nilcore-no-such-mcp-xyz"}}); err == nil {
		t.Error("missing binary must error, not hang")
	}
}

// TestManagerStdioReuseClose drives the Manager over a REAL stdio subprocess — the
// exact host-side path that closes the container gap: spawn once, reuse the live
// connection across calls, and tear the process down on Close.
func TestManagerStdioReuseClose(t *testing.T) {
	t.Setenv("NILCORE_MCP_MOCK", "1")
	m := NewManager(Config{Servers: []ServerSpec{
		{Name: "mock", Command: []string{os.Args[0], "-test.run=TestHelperMCPServer"}},
	}})
	ctx := context.Background()
	for i := 0; i < 3; i++ { // a single subprocess answers all three (reuse)
		out, err := m.CallTool(ctx, "mock", "search", json.RawMessage(`{}`))
		if err != nil || out != "echoed-ok" {
			t.Fatalf("CallTool[%d] = %q, %v", i, out, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close is idempotent.
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestLoadConfigHTTP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	js := `{"servers":[{"name":"remote","url":"https://mcp.example.com/v1","headers":{"Authorization":"Bearer t"}}]}`
	if err := os.WriteFile(path, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	spec, ok := cfg.Server("remote")
	if !ok || spec.URL == "" || spec.stdio() {
		t.Fatalf("remote spec = %+v, ok=%v (want non-stdio with URL)", spec, ok)
	}
	if spec.Headers["Authorization"] != "Bearer t" {
		t.Errorf("headers not parsed: %+v", spec.Headers)
	}
}
