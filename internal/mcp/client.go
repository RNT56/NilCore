// Package mcp connects MCP (Model Context Protocol) servers as typed code APIs on
// the sandbox filesystem — Anthropic's "code execution with MCP" model (P1-T09).
// Rather than loading every tool definition into context, the client lists a
// server's tools once and generates deterministic wrappers under
// ./mcp/servers/<server>/<tool>; the executor discovers them on demand with its
// read/search tools. A tool is then invoked HOST-SIDE through the native `mcp` tool
// (cmd/nilcore wires it over a Manager) so it works the same on every sandbox tier —
// including the macOS container default, where a binary baked into the box could not.
//
// Two MCP transports are supported (see transport.go): local stdio subprocesses and
// remote Streamable HTTP servers. tools, and optionally resources + prompts, are
// reachable. Implemented over JSON-RPC 2.0 in the standard library — no external
// dependency (invariant I6).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrToolFailed marks a tool-LEVEL failure (the server returned isError=true) as
// distinct from a transport/connection error, so the Manager retries a dead
// connection but never re-runs a tool that genuinely failed (which could repeat a
// side effect).
var ErrToolFailed = errors.New("mcp tool failed")

// Tool is an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Resource is an MCP resource descriptor (opt-in surface).
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
}

// Prompt is an MCP prompt descriptor (opt-in surface).
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Client speaks JSON-RPC 2.0 to one MCP server over a transport (stdio or HTTP).
type Client struct {
	Server string

	t transport

	mu     sync.Mutex
	nextID int
}

// NewClient wires a client to a transport.
func NewClient(server string, t transport) *Client {
	return &Client{Server: server, t: t}
}

// Close closes the transport.
func (c *Client) Close() error {
	if c.t != nil {
		return c.t.Close()
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	resp, err := c.t.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp %s: %s", method, resp.Error.Message)
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.t.notify(ctx, rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) error {
	var res json.RawMessage
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nilcore", "version": "0.1"},
	}, &res)
	if err != nil {
		return err
	}
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

// ListTools returns the server's tools (used once to generate wrappers).
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool invokes a tool and returns its concatenated text content. (Authorization
// is enforced upstream: servers are operator-configured, never model-emitted, and the
// model selects only a tool + JSON args, both fenced as untrusted data — I7.)
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var res struct {
		Content []contentBlock `json:"content"`
		IsError bool           `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", err
	}
	out := joinText(res.Content)
	// Per the MCP spec a tool-level failure is reported with isError=true and the
	// error detail in content (NOT as a JSON-RPC error). Surface it as a Go error so
	// the executor treats it as a failed tool call rather than a successful result.
	if res.IsError {
		return "", fmt.Errorf("%w: %s/%s: %s", ErrToolFailed, c.Server, name, tailText(out, 500))
	}
	return out, nil
}

// ListResources returns the server's resources (opt-in surface).
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var res struct {
		Resources []Resource `json:"resources"`
	}
	if err := c.call(ctx, "resources/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Resources, nil
}

// ReadResource fetches a resource by URI and returns its concatenated text contents.
// Binary (blob) contents are omitted — only text parts are returned (I7: data only).
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	var res struct {
		Contents []contentBlock `json:"contents"`
	}
	if err := c.call(ctx, "resources/read", map[string]any{"uri": uri}, &res); err != nil {
		return "", err
	}
	return joinText(res.Contents), nil
}

// ListPrompts returns the server's prompts (opt-in surface).
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	var res struct {
		Prompts []Prompt `json:"prompts"`
	}
	if err := c.call(ctx, "prompts/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Prompts, nil
}

// GetPrompt renders a named prompt (with optional args) to text. Each message's text
// content is concatenated; a prompt is INERT data (I7) — returned for the model to
// read, never auto-executed as an instruction.
func (c *Client) GetPrompt(ctx context.Context, name string, args json.RawMessage) (string, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	var res struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string       `json:"role"`
			Content contentBlock `json:"content"`
		} `json:"messages"`
	}
	if err := c.call(ctx, "prompts/get", params, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	if res.Description != "" {
		b.WriteString(res.Description)
		b.WriteString("\n\n")
	}
	for _, m := range res.Messages {
		if m.Content.Text != "" {
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content.Text)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// contentBlock is the shared MCP content shape (tool result / resource / prompt
// message). Only text is consumed; non-text parts (images, blobs) are ignored.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func joinText(blocks []contentBlock) string {
	var out strings.Builder
	for _, b := range blocks {
		out.WriteString(b.Text)
	}
	return out.String()
}

// tailText returns at most n characters of s (the trailing part when longer), so a
// surfaced tool error stays bounded.
func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
