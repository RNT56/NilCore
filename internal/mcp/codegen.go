package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// wrapperDir is where a server's tool wrappers live on the sandbox filesystem.
func wrapperDir(base, server string) string {
	return filepath.Join(base, "mcp", "servers", server)
}

// PruneServers removes the wrapper dir of every MCP server under base/mcp/servers/
// whose name is not in keep, so a server dropped from mcp.json leaves no stale,
// still-discoverable tool descriptors behind. A missing servers/ dir is a no-op
// (nothing was generated yet). The keep set is the live server names.
func PruneServers(base string, keep map[string]bool) error {
	root := filepath.Join(base, "mcp", "servers")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("mcp prune servers list %s: %w", root, err)
	}
	for _, e := range ents {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return fmt.Errorf("mcp prune stale server %s: %w", e.Name(), err)
		}
	}
	return nil
}

// GenerateWrappers writes one deterministic descriptor per tool under
// base/mcp/servers/<server>/<tool>.json. The descriptors are codegen (not
// model-written): each carries the tool's schema and how to invoke it, so the
// executor can discover a tool on demand (read/search) and call it via the host-
// dispatched `mcp` tool — without every definition being loaded into context up front.
//
// Regeneration is a full reconcile, not an append: after writing the current tool
// set it PRUNES any stale <tool>.json the dir still holds for a tool the server has
// since removed or renamed. Without this, a removed tool's descriptor would linger
// and stay discoverable (failing only at call time) — the discovery surface must
// reflect the live tool set.
func GenerateWrappers(base, server string, tools []Tool) error {
	dir := wrapperDir(base, server)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mcp wrapper dir: %w", err)
	}
	want := make(map[string]bool, len(tools))
	for _, t := range tools {
		desc := map[string]any{
			"server":      server,
			"tool":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"invoke": fmt.Sprintf(
				`call the "mcp" tool: {"server":%q,"tool":%q,"args":{…match inputSchema…}}`, server, t.Name),
		}
		fname := t.Name + ".json"
		if err := writeDescriptor(filepath.Join(dir, fname), desc); err != nil {
			return err
		}
		want[fname] = true
	}
	return pruneStaleWrappers(dir, want)
}

// pruneStaleWrappers removes every *.json descriptor in dir that is not in the
// desired set, so a regenerate reflects exactly the server's current tools. It skips
// subdirectories (e.g. resources/), so a server's resource descriptors are untouched.
// A read error on the dir is reported; an individual remove failure is fatal so a
// stale descriptor never silently survives.
func pruneStaleWrappers(dir string, want map[string]bool) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("mcp prune list %s: %w", dir, err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || want[name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("mcp prune stale wrapper %s: %w", name, err)
		}
	}
	return nil
}

// GenerateResourceWrappers writes one descriptor per resource under
// base/mcp/servers/<server>/resources/. Opt-in (NILCORE_MCP_RESOURCES); a resource is
// read via the `mcp` tool's resource arg.
func GenerateResourceWrappers(base, server string, resources []Resource) error {
	if len(resources) == 0 {
		return nil
	}
	dir := filepath.Join(wrapperDir(base, server), "resources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mcp resource dir: %w", err)
	}
	for _, r := range resources {
		desc := map[string]any{
			"server":      server,
			"resource":    r.URI,
			"name":        r.Name,
			"description": r.Description,
			"mimeType":    r.MIMEType,
			"invoke":      fmt.Sprintf(`call the "mcp" tool: {"server":%q,"resource":%q}`, server, r.URI),
		}
		if err := writeDescriptor(filepath.Join(dir, slug(r.Name, r.URI)+".json"), desc); err != nil {
			return err
		}
	}
	return nil
}

// GeneratePromptWrappers writes one descriptor per prompt under
// base/mcp/servers/<server>/prompts/. Opt-in; rendered via the `mcp` tool's prompt arg.
func GeneratePromptWrappers(base, server string, prompts []Prompt) error {
	if len(prompts) == 0 {
		return nil
	}
	dir := filepath.Join(wrapperDir(base, server), "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mcp prompt dir: %w", err)
	}
	for _, p := range prompts {
		desc := map[string]any{
			"server":      server,
			"prompt":      p.Name,
			"description": p.Description,
			"invoke":      fmt.Sprintf(`call the "mcp" tool: {"server":%q,"prompt":%q,"args":{…}}`, server, p.Name),
		}
		if err := writeDescriptor(filepath.Join(dir, slug(p.Name, p.Name)+".json"), desc); err != nil {
			return err
		}
	}
	return nil
}

func writeDescriptor(path string, desc map[string]any) error {
	b, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write descriptor %s: %w", filepath.Base(path), err)
	}
	return nil
}

// slug derives a filesystem-safe descriptor name from a preferred label, falling back
// to a sanitized form of an alternate (e.g. a resource URI) when the label is empty.
func slug(prefer, alt string) string {
	s := strings.TrimSpace(prefer)
	if s == "" {
		s = alt
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "resource"
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}
