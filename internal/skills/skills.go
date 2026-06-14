// Package skills adds capabilities as plugins, not core changes (P5-T01): it
// loads Agent Skills (SKILL.md — frontmatter name/description + a markdown body of
// instructions) and native tool plugins, and exposes both through the SAME tool
// registry the native loop already uses (so the frozen core never changes). A
// skill is surfaced as a tool that, when invoked, returns its instructions —
// on-demand guidance, like MCP-as-code keeps unused capability out of context.
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
// instructions surfaced to the model on demand.
type Skill struct {
	Name         string
	Description  string
	Instructions string
}

// Plugin is a native capability: it contributes a structured tool directly.
type Plugin interface {
	Tool() tools.Tool
}

// Registry holds loaded skills and native plugins.
type Registry struct {
	skills  []Skill
	plugins []Plugin
}

// New builds a registry from skills and plugins.
func New(skills []Skill, plugins ...Plugin) *Registry {
	return &Registry{skills: skills, plugins: plugins}
}

// AddPlugin registers a native plugin.
func (r *Registry) AddPlugin(p Plugin) { r.plugins = append(r.plugins, p) }

// AsTools exposes every skill and plugin as a tools.Tool, so they register into
// the native loop's registry exactly like built-in and MCP tools.
func (r *Registry) AsTools() []tools.Tool {
	out := make([]tools.Tool, 0, len(r.skills)+len(r.plugins))
	for _, s := range r.skills {
		out = append(out, skillTool{s})
	}
	for _, p := range r.plugins {
		out = append(out, p.Tool())
	}
	return out
}

// skillTool adapts a Skill into a tool whose invocation returns its instructions.
type skillTool struct{ s Skill }

func (t skillTool) Name() string { return "skill_" + t.s.Name }
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
		}
	}
	s.Instructions = strings.TrimSpace(strings.Join(lines[i:], "\n"))
	if s.Name == "" {
		return Skill{}, fmt.Errorf("frontmatter missing name")
	}
	return s, nil
}
