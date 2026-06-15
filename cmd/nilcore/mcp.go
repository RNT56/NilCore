package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nilcore/internal/mcp"
)

// mcpConfigPath resolves the MCP server config file: $NILCORE_MCP_CONFIG, else
// <workdir>/mcp.json. Servers are declared as {name, command} — the command (e.g.
// a stdio MCP server binary) is operator-configured, never model-emitted.
func mcpConfigPath(workdir string) string {
	if p := os.Getenv("NILCORE_MCP_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(workdir, "mcp.json")
}

// mcpCallMain implements `nilcore mcp-call <server> <tool> ['<json-args>']` — the
// runtime bridge the generated MCP wrappers invoke (mcp/codegen.go writes exactly
// this invoke string). It connects to the configured stdio server, calls the tool,
// and writes the textual result to stdout. The executor reads that output through
// its `run` tool, where the loop fences it as untrusted before it re-enters the
// model's context (I7); the call itself passes the loop's command-policy gate
// (P2-T04) when the model runs `nilcore mcp-call …`. Errors go to stderr + a
// non-zero exit, so a transient server failure surfaces as a command failure
// rather than crashing the loop.
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

// setupMCP generates the on-demand MCP tool wrappers under workdir/mcp/servers/ for
// each configured server, so the executor can discover them with its read/search
// tools (only what it opens reaches context). Best-effort: an absent config or a
// server that will not start is logged and skipped, never blocking the task.
func setupMCP(workdir string) {
	cfg, err := mcp.LoadConfig(mcpConfigPath(workdir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: mcp config: %v\n", err)
		return
	}
	if len(cfg.Servers) == 0 {
		return
	}
	ctx := context.Background()
	for _, spec := range cfg.Servers {
		if err := mcp.GenerateServer(ctx, workdir, spec); err != nil {
			fmt.Fprintf(os.Stderr, "nilcore: skipping mcp server %q: %v\n", spec.Name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "nilcore: generated mcp wrappers for %q under %s/mcp/servers/\n", spec.Name, workdir)
	}
}
