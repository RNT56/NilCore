package mcp

import (
	"crypto/sha256"
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
	taken := newSlugSet()
	for _, t := range tools {
		desc := map[string]any{
			"server":      server,
			"tool":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"invoke": fmt.Sprintf(
				`call the "mcp" tool: {"server":%q,"tool":%q,"args":{…match inputSchema…}}`, server, t.Name),
		}
		// t.Name is UNTRUSTED server output (tools/list) — treat it as data (I7). slug()
		// strips every path separator (→ '_'), so the filename can never traverse out of
		// dir (mirrors the resource/prompt paths below; the operator-trusted registry name
		// is hardened the same way via singleSegment). slug() is non-injective (many runes
		// fold to '_'), so DISTINCT tool names can collide to one base — uniqueSlug then
		// disambiguates with a short hash of the original name so no tool's descriptor is
		// silently overwritten. The descriptor's "tool" field keeps the ORIGINAL name so
		// the model still invokes the correct tool.
		fname := taken.assign(t.Name, t.Name) + ".json"
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
		if os.IsNotExist(err) {
			return nil // nothing was generated yet — nothing to prune
		}
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
// read via the `mcp` tool's resource arg. Like GenerateWrappers this is a full reconcile:
// after writing the current set it PRUNES any stale descriptor for a resource the server has
// since removed or renamed, so a dropped resource can't linger and stay discoverable. An
// empty set prunes everything (a server that dropped all its resources leaves none behind).
func GenerateResourceWrappers(base, server string, resources []Resource) error {
	dir := filepath.Join(wrapperDir(base, server), "resources")
	if len(resources) == 0 {
		return pruneStaleWrappers(dir, nil) // no resources: drop any left from a prior generation
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mcp resource dir: %w", err)
	}
	want := make(map[string]bool, len(resources))
	taken := newSlugSet()
	for _, r := range resources {
		desc := map[string]any{
			"server":      server,
			"resource":    r.URI,
			"name":        r.Name,
			"description": r.Description,
			"mimeType":    r.MIMEType,
			"invoke":      fmt.Sprintf(`call the "mcp" tool: {"server":%q,"resource":%q}`, server, r.URI),
		}
		// Key disambiguation on the URI (globally unique per resource) so two resources
		// whose display names slug alike don't overwrite each other.
		fname := taken.assign(slug(r.Name, r.URI), r.URI) + ".json"
		if err := writeDescriptor(filepath.Join(dir, fname), desc); err != nil {
			return err
		}
		want[fname] = true
	}
	return pruneStaleWrappers(dir, want)
}

// GeneratePromptWrappers writes one descriptor per prompt under
// base/mcp/servers/<server>/prompts/. Opt-in; rendered via the `mcp` tool's prompt arg. Like
// GenerateWrappers this is a full reconcile: after writing the current set it PRUNES any stale
// descriptor for a prompt the server has since removed or renamed, so a dropped prompt can't
// linger and stay discoverable. An empty set prunes everything.
func GeneratePromptWrappers(base, server string, prompts []Prompt) error {
	dir := filepath.Join(wrapperDir(base, server), "prompts")
	if len(prompts) == 0 {
		return pruneStaleWrappers(dir, nil) // no prompts: drop any left from a prior generation
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mcp prompt dir: %w", err)
	}
	want := make(map[string]bool, len(prompts))
	taken := newSlugSet()
	for _, p := range prompts {
		desc := map[string]any{
			"server":      server,
			"prompt":      p.Name,
			"description": p.Description,
			"invoke":      fmt.Sprintf(`call the "mcp" tool: {"server":%q,"prompt":%q,"args":{…}}`, server, p.Name),
		}
		fname := taken.assign(p.Name, p.Name) + ".json"
		if err := writeDescriptor(filepath.Join(dir, fname), desc); err != nil {
			return err
		}
		want[fname] = true
	}
	return pruneStaleWrappers(dir, want)
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

// slugSet allocates collision-free descriptor filenames. slug() is non-injective — many
// distinct labels fold to the same base (every illegal rune → '_') — so without this two
// different tool/resource/prompt names could produce one filename and the second write
// would silently clobber the first, dropping a tool from the on-disk descriptor set.
type slugSet struct {
	used map[string]string // base slug -> the ORIGINAL identity that first claimed it
}

func newSlugSet() *slugSet { return &slugSet{used: map[string]string{}} }

// assign returns a unique, filesystem-safe base name for (prefer, alt). It slugs the
// label, then — if that base was already taken by a DIFFERENT original identity — appends
// a short deterministic hash of this identity so the two never collide. Re-assigning the
// SAME identity (e.g. an idempotent regen of the same tool) returns the same base, so the
// descriptor set stays stable across runs.
func (s *slugSet) assign(prefer, alt string) string {
	base := slug(prefer, alt)
	identity := prefer + "\x00" + alt
	if owner, ok := s.used[base]; ok && owner != identity {
		// Collision with a different name: disambiguate deterministically. Trim so the
		// base+suffix stays within the 100-char cap slug() enforces.
		sum := sha256.Sum256([]byte(identity))
		suffix := "_" + fmt.Sprintf("%x", sum)[:8]
		trimmed := base
		if len(trimmed)+len(suffix) > 100 {
			trimmed = trimmed[:100-len(suffix)]
		}
		base = trimmed + suffix
		// The disambiguated base is derived from a hash, so a second-order collision is
		// astronomically unlikely; still record it so an exact re-run is idempotent.
	}
	s.used[base] = identity
	return base
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
