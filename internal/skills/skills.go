// Package skills adds capabilities as plugins, not core changes (P5-T01): it
// loads Agent Skills (SKILL.md — frontmatter name/description + a markdown body of
// instructions) and exposes them through the SAME tool registry the native loop
// already uses (so the frozen core never changes). A skill is surfaced as a tool
// that, when invoked, returns its instructions — on-demand guidance, like
// MCP-as-code keeps unused capability out of context.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"nilcore/internal/tools"
)

// Skill is an Agent Skill: a name, a one-line description, and a body of
// instructions surfaced to the model on demand. Version is optional metadata from
// the SKILL.md frontmatter (a semver-ish string), used by the registry (P10-T06)
// to track and update installed skills; empty when absent (byte-identical for
// existing skills).
type Skill struct {
	Name         string
	Description  string
	Instructions string
	Version      string
}

// Registry holds loaded skills.
type Registry struct {
	skills []Skill
}

// New builds a registry from skills.
func New(skills []Skill) *Registry {
	return &Registry{skills: skills}
}

// AsTools exposes every skill as a tools.Tool, so they register into the native
// loop's registry exactly like built-in and MCP tools.
func (r *Registry) AsTools() []tools.Tool {
	out := make([]tools.Tool, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, skillTool{s})
	}
	return out
}

// skillTool adapts a Skill into a tool whose invocation returns its instructions.
type skillTool struct{ s Skill }

func (t skillTool) Name() string { return skillNamePrefix + t.s.Name }
func (t skillTool) Description() string {
	return t.s.Description + " (returns step-by-step instructions)"
}
func (t skillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t skillTool) Run(context.Context, string, json.RawMessage) (string, error) {
	return t.s.Instructions, nil
}

// LoadDir discovers Agent Skills: any SKILL.md under dir (recursively).
func LoadDir(dir string) ([]Skill, error) {
	var out []Skill
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		s, perr := parseSkill(string(b))
		if perr != nil {
			return fmt.Errorf("%s: %w", path, perr)
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseSkill parses SKILL.md: a "--- key: value ... ---" frontmatter block
// followed by the instruction body.
func parseSkill(text string) (Skill, error) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Skill{}, fmt.Errorf("missing frontmatter")
	}
	var s Skill
	i := 1
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			i++
			break
		}
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			s.Name = strings.TrimSpace(v)
		case "description":
			s.Description = strings.TrimSpace(v)
		case "version":
			s.Version = strings.TrimSpace(v)
		}
	}
	s.Instructions = strings.TrimSpace(strings.Join(lines[i:], "\n"))
	if s.Name == "" {
		return Skill{}, fmt.Errorf("frontmatter missing name")
	}
	// The skill name flows into an API tool name ("skill_"+Name), which the
	// provider requires to match ^[a-zA-Z0-9_-]{1,64}$. A frontmatter name with a
	// space, non-ASCII rune, or excessive length would otherwise make EVERY model
	// call 400 (skills load into every primary loop). Deterministically slugify to
	// the safe charset, budgeting for the 6-char "skill_" prefix. If nothing
	// survives (e.g. an all-emoji name), reject with a clear error rather than emit
	// a poison tool name.
	safe := sanitizeSkillName(s.Name)
	if safe == "" {
		return Skill{}, fmt.Errorf("skill name %q has no characters valid for an API tool name (allowed: a-z A-Z 0-9 _ -)", s.Name)
	}
	s.Name = safe
	return s, nil
}

// skillNamePrefix is prepended to every skill's tool name by skillTool.Name.
const skillNamePrefix = "skill_"

// maxSkillNameLen is the length budget for a slugified skill name so that
// skillNamePrefix+Name stays within the provider's 64-char tool-name limit.
const maxSkillNameLen = 64 - len(skillNamePrefix)

// sanitizeSkillName maps an arbitrary frontmatter name onto the API tool-name
// charset (a-z A-Z 0-9 _ -), collapsing every other rune to a single '-', and
// truncates to the length budget. It returns "" when nothing valid remains, so
// the caller can reject rather than emit a name that poisons every model call.
func sanitizeSkillName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		// Collapse runs of invalid runes into a single '-' separator.
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxSkillNameLen {
		out = strings.Trim(out[:maxSkillNameLen], "-")
	}
	return out
}
