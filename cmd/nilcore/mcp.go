package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nilcore/internal/guard"
	"nilcore/internal/mcp"
)

// envMCPResources opts INTO the resources + prompts surface (off by default). When
// set, setupMCP also generates resource/prompt descriptors and the `mcp` tool honors
// the resource/prompt request shapes; otherwise it is tools-only.
const envMCPResources = "NILCORE_MCP_RESOURCES"

func mcpResourcesEnabled() bool { return strings.TrimSpace(os.Getenv(envMCPResources)) != "" }

// mcpMgr is the process-wide live MCP connection manager (host-side), set by setupMCP
// when servers are configured and read by buildBackend to register the `mcp` tool. nil
// ⇒ no MCP configured ⇒ the tool is never advertised (byte-identical).
var mcpMgr *mcp.Manager

// mcpConfigPath resolves the MCP server config file: $NILCORE_MCP_CONFIG, else
// <workdir>/mcp.json. Servers are declared as {name, command} (stdio) or {name, url}
// (Streamable HTTP) — operator-configured, never model-emitted.
func mcpConfigPath(workdir string) string {
	if p := os.Getenv("NILCORE_MCP_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(workdir, "mcp.json")
}

// setupMCP loads the MCP config, opens a live Manager over the configured servers, and
// generates the on-demand descriptors under workdir/mcp/servers/ (warming the
// connections the `mcp` tool then reuses). It returns the Manager so the caller can
// `defer mcpClose(...)`; nil when no servers are configured. Best-effort: a bad config
// or a server that will not start is logged and skipped, never blocking the task.
func setupMCP(workdir string) *mcp.Manager {
	cfg, err := mcp.LoadConfig(mcpConfigPath(workdir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: mcp config: %v\n", err)
		return nil
	}
	// Reconcile the discovery surface with the live config: prune the wrapper dir of
	// any server no longer in mcp.json BEFORE (re)generating, so a removed server leaves
	// no stale, still-discoverable tools behind. Runs even when the config is now empty
	// (so emptying mcp.json clears every server). Best-effort, logged not fatal.
	keep := make(map[string]bool, len(cfg.Servers))
	for _, spec := range cfg.Servers {
		keep[spec.Name] = true
	}
	if err := mcp.PruneServers(workdir, keep); err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: mcp prune stale servers: %v\n", err)
	}
	if len(cfg.Servers) == 0 {
		return nil
	}
	mgr := mcp.NewManager(cfg)
	for _, e := range mgr.Discover(context.Background(), workdir, mcpResourcesEnabled()) {
		fmt.Fprintf(os.Stderr, "nilcore: skipping mcp server %v\n", e)
	}
	mcpMgr = mgr
	fmt.Fprintf(os.Stderr, "nilcore: mcp ready (%d server(s)); descriptors under %s/mcp/servers/\n", len(cfg.Servers), workdir)
	return mgr
}

// mcpClose tears down the Manager's live server connections. nil-safe.
func mcpClose(m *mcp.Manager) {
	if m != nil {
		_ = m.Close()
	}
}

// mcpTool is the host-dispatched native tool the model calls to reach a configured MCP
// server. Running it HOST-SIDE (like the structured read/write/git tools) is what makes
// MCP work on EVERY sandbox tier — including the macOS container default, where the
// nilcore binary and the server runtime are not inside the box. The server set is
// operator-configured (mcp.json); the model only picks server + tool/resource/prompt +
// JSON args, all carried as data (I7) and audited by the loop.
type mcpTool struct{ mgr *mcp.Manager }

func newMCPTool(mgr *mcp.Manager) *mcpTool { return &mcpTool{mgr: mgr} }

func (t *mcpTool) Name() string { return "mcp" }

func (t *mcpTool) Description() string {
	d := "Call a configured MCP server's tool. Discover servers + tools by reading " +
		"./mcp/servers/<server>/<tool>.json. Input: {\"server\":\"<name>\",\"tool\":\"<tool>\",\"args\":{…}}."
	if mcpResourcesEnabled() {
		d += " Resources/prompts are enabled: also {\"server\",\"resource\":\"<uri>\"} or " +
			"{\"server\",\"prompt\":\"<name>\",\"args\":{…}} (descriptors under .../resources|prompts/)."
	}
	return d
}

func (t *mcpTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"server":{"type":"string","description":"configured MCP server name"},` +
		`"tool":{"type":"string","description":"tool to call"},` +
		`"args":{"type":"object","description":"JSON arguments matching the tool's inputSchema"},` +
		`"resource":{"type":"string","description":"resource URI to read (if enabled)"},` +
		`"prompt":{"type":"string","description":"prompt name to render (if enabled)"}` +
		`},"required":["server"]}`)
}

func (t *mcpTool) Run(ctx context.Context, _ string, input json.RawMessage) (string, error) {
	var in struct {
		Server   string          `json:"server"`
		Tool     string          `json:"tool"`
		Args     json.RawMessage `json:"args"`
		Resource string          `json:"resource"`
		Prompt   string          `json:"prompt"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("mcp: bad input: %w", err)
	}
	if strings.TrimSpace(in.Server) == "" {
		return "", fmt.Errorf("mcp: 'server' is required")
	}
	if t.mgr == nil {
		return "", fmt.Errorf("mcp: no servers configured")
	}
	switch {
	case in.Resource != "":
		if !mcpResourcesEnabled() {
			return "", fmt.Errorf("mcp: resources are not enabled (set %s=1)", envMCPResources)
		}
		return fenceMCPErr(t.mgr.ReadResource(ctx, in.Server, in.Resource))
	case in.Prompt != "":
		if !mcpResourcesEnabled() {
			return "", fmt.Errorf("mcp: prompts are not enabled (set %s=1)", envMCPResources)
		}
		return fenceMCPErr(t.mgr.GetPrompt(ctx, in.Server, in.Prompt, in.Args))
	case in.Tool != "":
		args := in.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		return fenceMCPErr(t.mgr.CallTool(ctx, in.Server, in.Tool, args))
	default:
		return "", fmt.Errorf("mcp: provide 'tool' (or 'resource'/'prompt' when enabled)")
	}
}

// fenceMCPErr fences an MCP call's ERROR text before it reaches the model. A server's
// error (tool isError content, an HTTP body snippet, a JSON-RPC message) is
// server-controlled and untrusted: the loop guard.Wraps the SUCCESS path, so the error
// path must fence the same untrusted content (I7) — otherwise a malicious server could
// smuggle instructions through a failed call. Harness validation errors above are not
// routed here, so only server-originated text is wrapped.
func fenceMCPErr(out string, err error) (string, error) {
	if err != nil {
		return "", errors.New(guard.Wrap("mcp error", err.Error()))
	}
	return out, nil
}

// mcpCallMain implements `nilcore mcp-call <server> <tool> ['<json-args>']` — the
// host-side CLI bridge (operator use, and the namespace-sandbox shell path). It is a
// one-shot connect→call→teardown over the configured transport (stdio or HTTP). The
// model's primary path is the native `mcp` tool above; this verb stays for operators
// and scripts. Errors go to stderr + a non-zero exit.
func mcpCallMain(args []string) {
	fs := flag.NewFlagSet("mcp-call", flag.ExitOnError)
	cfgPath := fs.String("mcp-config", "", "MCP servers config (default: $NILCORE_MCP_CONFIG or ./mcp.json)")
	dir := fs.String("dir", ".", "directory holding mcp.json (default config location)")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: nilcore mcp-call <server> <tool> ['<json-args>']")
		os.Exit(2)
	}
	server, tool := rest[0], rest[1]
	rawArgs := json.RawMessage(`{}`)
	if len(rest) >= 3 && strings.TrimSpace(rest[2]) != "" {
		rawArgs = json.RawMessage(rest[2])
	}

	path := *cfgPath
	if path == "" {
		path = mcpConfigPath(mustAbs(*dir))
	}
	cfg, err := mcp.LoadConfig(path)
	if err != nil {
		fatal(err)
	}
	spec, ok := cfg.Server(server)
	if !ok {
		fatal(fmt.Errorf("mcp server %q not configured in %s", server, path))
	}

	out, err := mcp.Call(context.Background(), spec, tool, rawArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-call %s/%s: %v\n", server, tool, err)
		os.Exit(1)
	}
	fmt.Print(out)
}
