package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// wrapperDir is where a server's tool wrappers live on the sandbox filesystem.
func wrapperDir(base, server string) string {
	return filepath.Join(base, "mcp", "servers", server)
}

// GenerateWrappers writes one deterministic descriptor per tool under
// base/mcp/servers/<server>/<tool>.json. The descriptors are codegen (not
// model-written): each carries the tool's schema and how to invoke it, so the
// executor can discover a tool on demand (read/search) and call it by writing
// code — without every definition being loaded into context up front.
func GenerateWrappers(base, server string, tools []Tool) error {
	dir := wrapperDir(base, server)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mcp wrapper dir: %w", err)
	}
	for _, t := range tools {
		desc := map[string]any{
			"server":      server,
			"tool":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"invoke":      fmt.Sprintf("nilcore mcp-call %s %s '<json-args>'", server, t.Name),
		}
		b, err := json.MarshalIndent(desc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, t.Name+".json"), b, 0o644); err != nil {
			return fmt.Errorf("write wrapper %s: %w", t.Name, err)
		}
	}
	return nil
}

// Discover lists the tool names available for a server by reading the generated
// wrapper directory — the on-demand discovery path the executor uses (only what
// it opens reaches context).
func Discover(base, server string) ([]string, error) {
	entries, err := os.ReadDir(wrapperDir(base, server))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name()[:len(e.Name())-len(".json")])
		}
	}
	sort.Strings(names)
	return names, nil
}
