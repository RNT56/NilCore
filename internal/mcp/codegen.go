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

// GenerateWrappers writes one deterministic descriptor per tool under
// base/mcp/servers/<server>/<tool>.json. The descriptors are codegen (not
// model-written): each carries the tool's schema and how to invoke it, so the
// executor can discover a tool on demand (read/search) and call it via the host-
// dispatched `mcp` tool — without every definition being loaded into context up front.
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
			"invoke": fmt.Sprintf(
				`call the "mcp" tool: {"server":%q,"tool":%q,"args":{…match inputSchema…}}`, server, t.Name),
		}
		if err := writeDescriptor(filepath.Join(dir, t.Name+".json"), desc); err != nil {
			return err
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
