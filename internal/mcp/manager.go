package mcp

// manager.go is the host-side MCP execution layer the native `mcp` tool runs over
// (cmd/nilcore wires it). It holds one live, initialized connection per configured
// server and REUSES it across calls, so a stdio subprocess is spawned once (session
// state persists) and an HTTP session id is kept — fixing the one-shot-per-call cost
// of the bare `Call` path. It is concurrency-safe (the native loop calls tools
// serially, but serve runs many sessions over one Manager) and recovers a dropped
// connection by evicting + reconnecting once.
//
// Trust boundary: every server here is OPERATOR-configured (mcp.json), never
// model-emitted; the model only selects a server + tool/resource/prompt + JSON args,
// all carried as data (I7). This is why dispatching MCP host-side is bounded — it is
// the operator's sanctioned server set, invoked only by its declared surface, the
// same place `setupMCP` already spawns servers for discovery.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Manager owns the live MCP connections for a process.
type Manager struct {
	cfg Config

	mu    sync.Mutex
	conns map[string]*conn
}

type conn struct {
	client *Client
	stop   func()
}

// NewManager builds a Manager over the configured servers. Connections are opened
// lazily on first use.
func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg, conns: map[string]*conn{}}
}

// Servers returns the configured server specs (read-only; for discovery wiring).
func (m *Manager) Servers() []ServerSpec { return m.cfg.Servers }

// get returns a live, initialized client for server, opening + caching it on first
// use. fresh reports whether it was opened on THIS call (so a caller knows a retry is
// pointless after a fresh open). The lock spans connect+initialize so two callers can
// never double-spawn the same server.
func (m *Manager) get(ctx context.Context, server string) (*Client, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.conns[server]; ok {
		return c.client, false, nil
	}
	spec, ok := m.cfg.Server(server)
	if !ok {
		return nil, false, fmt.Errorf("unknown mcp server %q", server)
	}
	client, stop, err := connect(ctx, spec)
	if err != nil {
		return nil, false, err
	}
	if err := client.Initialize(ctx); err != nil {
		stop()
		return nil, false, fmt.Errorf("mcp init %q: %w", server, err)
	}
	m.conns[server] = &conn{client: client, stop: stop}
	return client, true, nil
}

// evict closes + drops a server's cached connection (after a transport failure), so
// the next call reconnects.
func (m *Manager) evict(server string) {
	m.mu.Lock()
	c, ok := m.conns[server]
	if ok {
		delete(m.conns, server)
	}
	m.mu.Unlock()
	if ok && c.stop != nil {
		c.stop()
	}
}

// withRetry runs op against a (possibly reused) connection and, on a TRANSPORT error
// from a reused connection, evicts + reconnects + retries once. A tool-LEVEL failure
// (ErrToolFailed) is never retried — that would risk repeating a side effect.
func (m *Manager) withRetry(ctx context.Context, server string, op func(*Client) (string, error)) (string, error) {
	c, fresh, err := m.get(ctx, server)
	if err != nil {
		return "", err
	}
	out, err := op(c)
	if err == nil || fresh || errors.Is(err, ErrToolFailed) {
		return out, err
	}
	m.evict(server)
	c, _, rerr := m.get(ctx, server)
	if rerr != nil {
		return "", rerr
	}
	return op(c)
}

// CallTool invokes a tool on a configured server, reusing the live connection.
func (m *Manager) CallTool(ctx context.Context, server, tool string, args json.RawMessage) (string, error) {
	return m.withRetry(ctx, server, func(c *Client) (string, error) {
		return c.CallTool(ctx, tool, args)
	})
}

// ReadResource reads a resource by URI from a configured server (opt-in surface).
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (string, error) {
	return m.withRetry(ctx, server, func(c *Client) (string, error) {
		return c.ReadResource(ctx, uri)
	})
}

// GetPrompt renders a named prompt from a configured server (opt-in surface).
func (m *Manager) GetPrompt(ctx context.Context, server, name string, args json.RawMessage) (string, error) {
	return m.withRetry(ctx, server, func(c *Client) (string, error) {
		return c.GetPrompt(ctx, name, args)
	})
}

// Discover lists each configured server's tools (and, when withResources, its
// resources + prompts) and writes the on-demand descriptors under base/mcp/servers/.
// It uses the Manager's own connections, so a server opened for discovery stays WARM
// for the subsequent tool calls (no spawn-twice). Best-effort per server: a server
// that won't start is returned in the error slice and skipped, never fatal.
func (m *Manager) Discover(ctx context.Context, base string, withResources bool) []error {
	var errs []error
	for _, spec := range m.cfg.Servers {
		c, _, err := m.get(ctx, spec.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.Name, err))
			continue
		}
		tools, err := c.ListTools(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.Name, err))
			continue
		}
		if err := GenerateWrappers(base, spec.Name, tools); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.Name, err))
			continue
		}
		if withResources {
			if res, rerr := c.ListResources(ctx); rerr == nil {
				_ = GenerateResourceWrappers(base, spec.Name, res)
			}
			if pr, perr := c.ListPrompts(ctx); perr == nil {
				_ = GeneratePromptWrappers(base, spec.Name, pr)
			}
		}
	}
	return errs
}

// Close tears down every live connection. Idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	conns := m.conns
	m.conns = map[string]*conn{}
	m.mu.Unlock()
	for _, c := range conns {
		if c.stop != nil {
			c.stop()
		}
	}
	return nil
}
