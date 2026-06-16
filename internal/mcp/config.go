package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ServerSpec declares how to reach one MCP server: a Name (used in the wrapper
// path and in `nilcore mcp-call`) and a Command that launches a stdio server
// (program + args). The command is OPERATOR-configured, never model-emitted.
type ServerSpec struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	// Version is optional metadata tracked by the registry (P10-T06); omitted when
	// absent so existing mcp.json files are byte-identical.
	Version string `json:"version,omitempty"`
}

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

// Spawn launches a stdio MCP server subprocess and wraps it as a Client. Returns
// the client and a stop func that closes the stream and reaps the process. A
// missing binary is a clean error (callers degrade gracefully, never hang).
func Spawn(ctx context.Context, spec ServerSpec) (*Client, func(), error) {
	if len(spec.Command) == 0 {
		return nil, nil, fmt.Errorf("mcp server %q has no command", spec.Name)
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
	client := NewClient(spec.Name, &processRW{w: stdin, r: stdout})
	stop := func() {
		_ = client.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return client, stop, nil
}

// Call is a one-shot: connect to spec, handshake, invoke tool with args, return the
// textual result, and tear down. This is what `nilcore mcp-call` runs.
func Call(ctx context.Context, spec ServerSpec, tool string, args json.RawMessage) (string, error) {
	client, stop, err := Spawn(ctx, spec)
	if err != nil {
		return "", err
	}
	defer stop()
	if err := client.Initialize(ctx); err != nil {
		return "", err
	}
	return client.CallTool(ctx, tool, args)
}

// GenerateServer connects to spec, lists its tools, and writes the on-demand
// wrappers under base/mcp/servers/<name>/ — the discovery surface the executor
// reads. Best-effort at the call site: a server that won't start returns an error
// the caller can log and skip.
func GenerateServer(ctx context.Context, base string, spec ServerSpec) error {
	client, stop, err := Spawn(ctx, spec)
	if err != nil {
		return err
	}
	defer stop()
	if err := client.Initialize(ctx); err != nil {
		return err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return err
	}
	return GenerateWrappers(base, spec.Name, tools)
}

// processRW bridges a subprocess's separate stdin (writer) and stdout (reader)
// into one io.ReadWriteCloser for the JSON-RPC transport.
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
