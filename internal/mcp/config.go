package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// ServerSpec declares how to reach one MCP server. EXACTLY ONE transport is given:
//   - Command: a local stdio server (program + args), launched as a subprocess.
//   - URL: a remote Streamable-HTTP server (operator-trusted endpoint).
//
// The spec is OPERATOR-configured (mcp.json), never model-emitted.
type ServerSpec struct {
	Name    string   `json:"name"`
	Command []string `json:"command,omitempty"`
	// URL selects the remote Streamable-HTTP transport. When set, Command is ignored.
	URL string `json:"url,omitempty"`
	// Headers are static HTTP headers (e.g. {"Authorization": "Bearer …"}) sent on
	// every request to an HTTP server. Resolve secrets into these host-side at config
	// load; they never reach the model (I3). Ignored for a stdio server.
	Headers map[string]string `json:"headers,omitempty"`
	// Version is optional metadata tracked by the registry (P10-T06); omitted when
	// absent so existing mcp.json files are byte-identical.
	Version string `json:"version,omitempty"`
}

// stdio reports whether this spec uses the local subprocess transport.
func (s ServerSpec) stdio() bool { return s.URL == "" }

// Config is the set of configured MCP servers, loaded from an mcp.json file.
type Config struct {
	Servers []ServerSpec `json:"servers"`
}

// LoadConfig reads the MCP server config at path. A missing file is not an error
// (no servers configured); a malformed one is.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read mcp config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse mcp config %s: %w", path, err)
	}
	return c, nil
}

// Server returns the spec named name, or ok=false.
func (c Config) Server(name string) (ServerSpec, bool) {
	for _, s := range c.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return ServerSpec{}, false
}

// httpClient is the shared client for HTTP MCP servers — a generous timeout that
// tolerates a long SSE response without hanging forever.
var httpClient = &http.Client{Timeout: 120 * time.Second}

// connect dials a server over its declared transport and returns a Client plus a stop
// func that tears the connection down. A stdio server is launched as a subprocess; an
// HTTP server needs no process, so its stop just closes idle connections. A missing
// binary / unreachable URL is a clean error (callers degrade gracefully, never hang).
func connect(ctx context.Context, spec ServerSpec) (*Client, func(), error) {
	if !spec.stdio() {
		t := newHTTPTransport(spec.URL, spec.Headers, httpClient)
		return NewClient(spec.Name, t), func() { _ = t.Close() }, nil
	}
	if len(spec.Command) == 0 {
		return nil, nil, fmt.Errorf("mcp server %q has neither a command nor a url", spec.Name)
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard // server diagnostics chatter is not the JSON-RPC channel
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start mcp server %q: %w", spec.Name, err)
	}
	client := NewClient(spec.Name, newStdioTransport(&processRW{w: stdin, r: stdout}))
	stop := func() {
		_ = client.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return client, stop, nil
}

// Spawn launches a server and wraps it as a Client (stdio subprocess or HTTP). Kept
// as the public entry; it delegates to connect.
func Spawn(ctx context.Context, spec ServerSpec) (*Client, func(), error) {
	return connect(ctx, spec)
}

// Call is a one-shot: connect to spec, handshake, invoke tool with args, return the
// textual result, and tear down. This is what `nilcore mcp-call` runs (host-side).
func Call(ctx context.Context, spec ServerSpec, tool string, args json.RawMessage) (string, error) {
	client, stop, err := connect(ctx, spec)
	if err != nil {
		return "", err
	}
	defer stop()
	if err := client.Initialize(ctx); err != nil {
		return "", err
	}
	return client.CallTool(ctx, tool, args)
}

// processRW bridges a subprocess's separate stdin (writer) and stdout (reader) into
// one io.ReadWriteCloser for the stdio transport.
type processRW struct {
	w io.WriteCloser
	r io.ReadCloser
}

func (p *processRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *processRW) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *processRW) Close() error {
	werr := p.w.Close()
	rerr := p.r.Close()
	if werr != nil {
		return werr
	}
	return rerr
}
