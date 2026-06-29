// Package mcp connects MCP (Model Context Protocol) servers as typed code APIs on
// the sandbox filesystem — Anthropic's "code execution with MCP" model (P1-T09).
// Rather than loading every tool definition into context, the client lists a
// server's tools once and generates deterministic wrappers under
// ./mcp/servers/<server>/<tool>; the executor discovers them on demand with its
// read/search tools and invokes/chains them by writing code that runs in the
// sandbox, so unused tools cost ~zero tokens. Authorization is enforced at the
// codegen-descriptor boundary the model invokes through (cmd/nilcore/mcp.go), and the
// glue runs under the injection guard (P2-T05). Implemented over JSON-RPC 2.0 in
// the standard library — no external dependency (invariant I6).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Tool is an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client speaks JSON-RPC 2.0 to one MCP server over a duplex stream.
type Client struct {
	Server string

	enc    *json.Encoder
	dec    *json.Decoder
	closer io.Closer
	nextID int
}

// NewClient wires a client to a duplex transport (stdio pipe, socket, …).
func NewClient(server string, rw io.ReadWriteCloser) *Client {
	return &Client{Server: server, enc: json.NewEncoder(rw), dec: json.NewDecoder(rw), closer: rw}
}

// Close closes the transport.
func (c *Client) Close() error {
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
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

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.nextID++
	id := c.nextID
	if err := c.enc.Encode(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return fmt.Errorf("mcp send %s: %w", method, err)
	}
	for {
		var resp rpcResponse
		if err := c.dec.Decode(&resp); err != nil {
			return fmt.Errorf("mcp recv %s: %w", method, err)
		}
		if resp.ID != id {
			continue // a notification or an unrelated message — skip it
		}
		if resp.Error != nil {
			return fmt.Errorf("mcp %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

func (c *Client) notify(method string, params any) error {
	return c.enc.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
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
	return c.notify("notifications/initialized", map[string]any{})
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
// is enforced upstream at the codegen-descriptor boundary the model actually calls
// through — cmd/nilcore/mcp.go — not here; this client is the low-level transport.)
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", err
	}
	var out string
	for _, b := range res.Content {
		out += b.Text
	}
	// Per the MCP spec a tool-level failure is reported with isError=true and the
	// error detail in content (NOT as a JSON-RPC error). Surface it as a Go error so
	// the executor treats it as a failed tool call rather than a successful result —
	// otherwise a failing MCP tool reads as success. The content carries the detail.
	if res.IsError {
		return "", fmt.Errorf("mcp tool %s/%s failed: %s", c.Server, name, tailText(out, 500))
	}
	return out, nil
}

// tailText returns at most n characters of s (the trailing part when longer), so a
// surfaced tool error stays bounded.
func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
