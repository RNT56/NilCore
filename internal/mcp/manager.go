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
	"time"
)

// connectTimeout bounds the SHARED connect+initialize handshake behind a single-flight
// reservation. It is detached from any individual caller's request ctx: the first caller
// starts the handshake, but its cancellation must never fail the OTHER callers waiting on
// the same reservation (each still honors its own ctx while it waits). Generous, so a
// slow-but-live server still comes up.
const connectTimeout = 30 * time.Second

// Manager owns the live MCP connections for a process.
type Manager struct {
	cfg Config

	// procCtx bounds every spawned subprocess to the MANAGER's lifetime, not a request's
	// — so a connection opened (or reconnected) mid-task survives past that task. Only
	// Close (via cancel) tears them down. The request ctx still bounds each round-trip.
	procCtx context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	conns map[string]*conn
}

type conn struct {
	client *Client
	stop   func()
	ready  chan struct{} // closed once client/stop (or err) are set — lets waiters block
	err    error         // connect/init failure, surfaced to waiters
}

// NewManager builds a Manager over the configured servers. Connections are opened
// lazily on first use and live until Close.
func NewManager(cfg Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, procCtx: ctx, cancel: cancel, conns: map[string]*conn{}}
}

// get returns a live, initialized client for server, opening + caching it on first use.
// fresh reports whether it was opened on THIS call (so a caller knows a retry is
// pointless after a fresh open). Connect + initialize run OUTSIDE the lock behind a
// single-flight reservation, so two callers for the same server never double-spawn while
// callers for OTHER servers (and cache hits) never block on a slow handshake.
func (m *Manager) get(ctx context.Context, server string) (*Client, bool, error) {
	m.mu.Lock()
	if c, ok := m.conns[server]; ok {
		m.mu.Unlock()
		// A concurrent in-flight connect may still be completing. Honor OUR ctx while we
		// wait: a slow server stuck on `initialize` (bounded by the DETACHED handshake ctx)
		// must not make every subsequent caller block on <-c.ready past its own deadline.
		// The handshake's own cancellation is independent of any caller, so one caller's
		// cancel never lands a context.Canceled in c.err for the others.
		select {
		case <-c.ready:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		if c.err != nil {
			return nil, false, c.err
		}
		return c.client, false, nil
	}
	spec, ok := m.cfg.Server(server)
	if !ok {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("unknown mcp server %q", server)
	}
	c := &conn{ready: make(chan struct{})} // reserve so concurrent callers wait, not race
	m.conns[server] = c
	m.mu.Unlock()

	// The shared connect+initialize runs under a DETACHED ctx (derived from the Manager's
	// procCtx, NOT this caller's request ctx) with its own bounded timeout. This is the
	// single-flight handshake every concurrent caller for this server waits on, so binding
	// it to the first caller's ctx would let THAT caller's cancellation set c.err to a
	// context.Canceled and fail unrelated waiters (which withRetry won't retry — it only
	// retries errDeliveryFailed). Each waiter still honors its OWN ctx while it blocks on
	// <-c.ready above; only the shared handshake itself is decoupled.
	hctx, hcancel := context.WithTimeout(m.procCtx, connectTimeout)
	defer hcancel()
	client, stop, err := connect(m.procCtx, spec) // subprocess bound to Manager lifetime
	if err == nil {
		if ierr := client.Initialize(hctx); ierr != nil { // detached handshake ctx
			stop()
			err = fmt.Errorf("mcp init %q: %w", server, ierr)
		}
	}
	if err != nil {
		c.err = err
		close(c.ready)
		m.mu.Lock()
		if cur, ok := m.conns[server]; ok && cur == c { // only drop our own reservation
			delete(m.conns, server)
		}
		m.mu.Unlock()
		return nil, false, err
	}
	c.client, c.stop = client, stop
	close(c.ready)
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

// withRetry runs op against a (possibly reused) connection and retries ONCE only when
// the request was provably NOT delivered (errDeliveryFailed — e.g. a cached stdio
// subprocess died between calls, so the send failed before the server saw anything) on a
// REUSED connection. Every server-RECEIVED failure — a tool error (ErrToolFailed), a
// JSON-RPC error, an HTTP non-2xx, or a dropped reply — is returned as-is and never
// re-run, since the side effect may already have executed.
func (m *Manager) withRetry(ctx context.Context, server string, op func(*Client) (string, error)) (string, error) {
	c, fresh, err := m.get(ctx, server)
	if err != nil {
		return "", err
	}
	out, err := op(c)
	if err == nil || fresh || !errors.Is(err, errDeliveryFailed) {
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

// ListTools returns a configured server's advertised tools (name, description,
// inputSchema), reusing the live connection. It backs the model's `list` discovery
// action: the descriptors GenerateWrappers writes live under the host descriptor base
// (a cache dir), which the model's worktree-rooted file tools cannot see, so discovery
// must go through the tool.
func (m *Manager) ListTools(ctx context.Context, server string) ([]Tool, error) {
	c, _, err := m.get(ctx, server)
	if err != nil {
		return nil, err
	}
	return c.ListTools(ctx)
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
				if werr := GenerateResourceWrappers(base, spec.Name, res); werr != nil {
					errs = append(errs, fmt.Errorf("%s resources: %w", spec.Name, werr))
				}
			}
			if pr, perr := c.ListPrompts(ctx); perr == nil {
				if werr := GeneratePromptWrappers(base, spec.Name, pr); werr != nil {
					errs = append(errs, fmt.Errorf("%s prompts: %w", spec.Name, werr))
				}
			}
		}
	}
	return errs
}

// Close tears down every live connection. Idempotent. Cancelling procCtx also kills any
// subprocess still mid-connect (so an in-flight reservation can never leak a process).
func (m *Manager) Close() error {
	if m.cancel != nil {
		m.cancel()
	}
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
