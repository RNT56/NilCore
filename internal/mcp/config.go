package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sync"
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
	// Headers are static HTTP headers sent on every request to an HTTP server. A value
	// may embed a secret PLACEHOLDER — {{secret:NAME}} (resolved via the SecretStore) or
	// {{env:NAME}} (resolved from the process environment) — so a bearer token is NEVER
	// required in plaintext on disk. Placeholders are resolved host-side at config load
	// (ResolveSecrets), so the literal token never reaches the model (I3). An unresolved
	// placeholder is a hard error, never silently sent as the literal text. Ignored for a
	// stdio server. Example: {"Authorization": "Bearer {{secret:MY_MCP_TOKEN}}"}.
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

// SecretResolver resolves a named secret host-side (e.g. the SecretStore). It is the
// seam through which header placeholders are filled without the mcp package depending
// on any concrete credential backend.
type SecretResolver interface {
	Get(name string) (string, error)
}

// placeholderRE matches a whole-value secret placeholder: {{secret:NAME}} or
// {{env:NAME}}. NAME is a conservative identifier so a header value can never smuggle a
// separator or whitespace through the lookup.
var placeholderRE = regexp.MustCompile(`\{\{(secret|env):([A-Za-z_][A-Za-z0-9_.-]*)\}\}`)

// ResolveSecrets fills every header placeholder ({{secret:NAME}} / {{env:NAME}}) in the
// config's HTTP servers, host-side, so no literal token has to live in mcp.json and none
// ever reaches the model (I3). {{secret:NAME}} goes through the resolver (SecretStore);
// {{env:NAME}} reads the process environment. An unresolved placeholder is a hard error
// — the literal is NEVER sent as-is (which would leak a placeholder as a bogus bearer
// token). A header with no placeholder is passed through verbatim (static values still
// work). A stdio server (no URL) has no headers to resolve.
//
// It mutates the receiver's specs in place and returns it for chaining. resolver may be
// nil only when no {{secret:…}} placeholder is present; a {{secret:…}} without a resolver
// is a clear error.
func (c Config) ResolveSecrets(resolver SecretResolver) (Config, error) {
	for i := range c.Servers {
		s := &c.Servers[i]
		if s.stdio() || len(s.Headers) == 0 {
			continue
		}
		for k, v := range s.Headers {
			resolved, err := resolveHeaderValue(v, resolver)
			if err != nil {
				return c, fmt.Errorf("mcp server %q header %q: %w", s.Name, k, err)
			}
			s.Headers[k] = resolved
		}
	}
	return c, nil
}

// resolveHeaderValue replaces every {{secret:NAME}}/{{env:NAME}} placeholder in v. A
// lookup failure (missing secret, absent env var, or a {{secret:…}} with no resolver) is
// returned as an error so an unresolved placeholder is never emitted as a literal.
func resolveHeaderValue(v string, resolver SecretResolver) (string, error) {
	var lookupErr error
	out := placeholderRE.ReplaceAllStringFunc(v, func(match string) string {
		m := placeholderRE.FindStringSubmatch(match)
		kind, name := m[1], m[2]
		switch kind {
		case "secret":
			if resolver == nil {
				lookupErr = fmt.Errorf("placeholder %s needs a secret store but none is configured", match)
				return match
			}
			val, err := resolver.Get(name)
			if err != nil {
				lookupErr = fmt.Errorf("resolve secret %q: %w", name, err)
				return match
			}
			return val
		case "env":
			val, ok := os.LookupEnv(name)
			if !ok || val == "" {
				lookupErr = fmt.Errorf("environment variable %q for placeholder %s is unset", name, match)
				return match
			}
			return val
		default:
			lookupErr = fmt.Errorf("unknown placeholder kind %q", kind)
			return match
		}
	})
	if lookupErr != nil {
		return "", lookupErr
	}
	return out, nil
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
	// reap kills + waits the child exactly once. sync.Once makes it safe to call from BOTH
	// the transport's Close (a ctx-cancelled round-trip tears the pipes down and reaps the
	// child so it is not left a zombie until the next call) AND stop below (Manager evict /
	// Close), which can race — two concurrent cmd.Wait() calls would otherwise be a bug.
	var reapOnce sync.Once
	reap := func() {
		reapOnce.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	client := NewClient(spec.Name, newStdioTransport(&processRW{w: stdin, r: stdout, reap: reap}))
	stop := func() {
		_ = client.Close() // closes the pipes and (via processRW.Close) reaps the child
		reap()             // idempotent: guarantees the child is reaped even on an odd teardown path
	}
	return client, stop, nil
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
// one io.ReadWriteCloser for the stdio transport. reap (optional) kills + waits the child;
// closing the pipes on a ctx-cancelled round-trip then also reaps the process, so the child
// is not left a zombie until the next call fails or the Manager tears the server down.
type processRW struct {
	w    io.WriteCloser
	r    io.ReadCloser
	reap func() // idempotent Kill+Wait of the child; nil for non-subprocess transports (tests)
}

func (p *processRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *processRW) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *processRW) Close() error {
	werr := p.w.Close()
	rerr := p.r.Close()
	if p.reap != nil {
		p.reap() // reap the child now (idempotent) so a cancelled round-trip leaves no zombie
	}
	if werr != nil {
		return werr
	}
	return rerr
}
