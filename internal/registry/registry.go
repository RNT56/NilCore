// Package registry adds a versioned, manifest-driven install layer over the
// existing local skills + MCP primitives (P10-T06). It turns operator-only,
// hand-placed capability into a tracked, updatable artifact: a manifest lists
// entries (name, kind, version, local source) and Install copies a skill into the
// discovery directory the loop already reads ($NILCORE_SKILLS_DIR /
// <config>/nilcore/skills).
//
// Trust boundary preserved:
//   - Sources are LOCAL paths. Remote fetch / a marketplace is deliberately OUT OF
//     SCOPE here — it grants the process network-fetch authority and belongs to the
//     external-infra roadmap (EXT-07), gated behind the §0 thesis gate.
//   - An installed skill is still a `skill_<name>` tool that only returns
//     instructions (no write surface). Install is the MECHANICS, not the gating.
//     The trusted path today is the operator typing `nilcore registry install` —
//     the human at the keyboard IS the approval. A FUTURE autonomous install path
//     (an agent deciding to add to its own skill set) must route through the
//     verified, human-gated self-edit flow (internal/selfimprove); that gate is the
//     caller's responsibility, not this package's.
//   - MCP-server install (writing mcp.json) is left to a follow-up; this package
//     installs skills, the lower-risk half.
//
// Stdlib only (invariant I6).
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nilcore/internal/skills"
)

// Kind enumerates what a manifest entry installs. Only KindSkill is implemented;
// KindMCP is reserved (mcp.json install is a follow-up).
const (
	KindSkill = "skill"
	KindMCP   = "mcp"
)

// Entry is one installable capability.
type Entry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`    // "skill" (implemented) | "mcp" (reserved)
	Version string `json:"version"` // semver-ish; tracked for updates
	Source  string `json:"source"`  // LOCAL path to the SKILL.md (no remote fetch — EXT-07)
}

// Manifest is a list of installable entries, loaded from a JSON file.
type Manifest struct {
	Entries []Entry `json:"entries"`
}

// LoadManifest reads a manifest file. A missing file yields an empty manifest
// (not an error) so the registry is opt-in.
func LoadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, nil
		}
		return Manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return m, nil
}

// Skills returns the skill entries from the manifest.
func (m Manifest) Skills() []Entry {
	var out []Entry
	for _, e := range m.Entries {
		if e.Kind == KindSkill {
			out = append(out, e)
		}
	}
	return out
}

// InstallSkill copies the entry's local source SKILL.md into
// skillsDir/<name>/SKILL.md and verifies it loads (rolling back if it does not).
// It is idempotent: re-installing the same content is a no-op overwrite. It does
// NOT fetch over the network and does NOT gate — gating belongs to the caller
// (operator-typed CLI today; the human-gated self-edit flow for a future
// autonomous path).
func InstallSkill(e Entry, skillsDir string) error {
	if e.Kind != KindSkill {
		return fmt.Errorf("registry: entry %q is kind %q, not a skill", e.Name, e.Kind)
	}
	if e.Name == "" || e.Source == "" {
		return fmt.Errorf("registry: skill entry needs a name and a local source")
	}
	// The entry name becomes a directory under skillsDir, so it must be a single
	// clean path segment — no separators, no "..", not absolute. A name like
	// "../../etc/x" would otherwise write a SKILL.md outside the discovery dir. The
	// manifest is operator-supplied/local-trusted, so this is defense-in-depth path
	// hardening, but it costs nothing and removes the traversal write entirely.
	if !singleSegment(e.Name) {
		return fmt.Errorf("registry: skill name %q must be a single path segment (no separators, no '..', not absolute)", e.Name)
	}
	src, err := os.ReadFile(e.Source)
	if err != nil {
		return fmt.Errorf("registry: read skill source %s: %w", e.Source, err)
	}

	destDir := filepath.Join(skillsDir, e.Name)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("registry: mkdir %s: %w", destDir, err)
	}
	dest := filepath.Join(destDir, "SKILL.md")
	// Preserve any prior-good skill at this name: a bad re-install (overwrite) must
	// NOT destroy the working skill it was replacing. If a SKILL.md already exists we
	// snapshot it and restore it on rollback; only a genuinely fresh install removes
	// the dir it created.
	prev, hadPrev := os.ReadFile(dest)
	if err := os.WriteFile(dest, src, 0o644); err != nil {
		return fmt.Errorf("registry: write %s: %w", dest, err)
	}

	// Verify the installed skill actually loads; roll back a bad install so the
	// discovery dir never holds an unparseable skill.
	loaded, err := skills.LoadDir(destDir)
	if err != nil || len(loaded) == 0 {
		if hadPrev == nil {
			_ = os.WriteFile(dest, prev, 0o644) // restore the prior-good skill
		} else {
			_ = os.RemoveAll(destDir) // fresh bad install — remove what we created
		}
		if err != nil {
			return fmt.Errorf("registry: installed skill %q does not load: %w", e.Name, err)
		}
		return fmt.Errorf("registry: installed skill %q produced no loadable skill", e.Name)
	}
	return nil
}

// singleSegment reports whether name is a single, clean path segment safe to join
// under a directory: non-empty, not absolute, not "." or "..", no separator (OS or
// '/'), and equal to its own filepath.Clean (which rejects embedded "..", "." and
// trailing slashes). The explicit "." / ".." reject is needed because both are
// their own filepath.Clean.
func singleSegment(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) {
		return false
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return false
	}
	return filepath.Clean(name) == name
}

// Installed lists the skills currently present in skillsDir (with their versions),
// so an operator/registry can see what is installed and decide on updates.
func Installed(skillsDir string) ([]skills.Skill, error) {
	if _, err := os.Stat(skillsDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return skills.LoadDir(skillsDir)
}
