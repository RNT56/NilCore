package skills_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/skills"
	"nilcore/internal/tools"
)

// examplePlugin is the native-plugin example: it contributes a structured tool.
type examplePlugin struct{}

func (examplePlugin) Tool() tools.Tool { return echoTool{} }

type echoTool struct{}

func (echoTool) Name() string            { return "echo" }
func (echoTool) Description() string     { return "echo the input" }
func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (echoTool) Run(_ context.Context, _ string, in json.RawMessage) (string, error) {
	return string(in), nil
}

func TestLoadSkillMarkdown(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "greet")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const md = "---\nname: greet\ndescription: Greet the user warmly\n---\nWhen greeting, be warm and concise. Use their name if known.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := skills.LoadDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadDir = %d, %v", len(loaded), err)
	}
	if loaded[0].Name != "greet" || loaded[0].Description != "Greet the user warmly" {
		t.Fatalf("parsed skill = %+v", loaded[0])
	}
	if loaded[0].Instructions == "" {
		t.Error("instruction body not parsed")
	}
}

func TestAsToolsExposesBothFormats(t *testing.T) {
	reg := skills.New(
		[]skills.Skill{{Name: "greet", Description: "greet", Instructions: "be warm"}},
		examplePlugin{},
	)
	ts := reg.AsTools()
	if len(ts) != 2 {
		t.Fatalf("AsTools = %d, want 2", len(ts))
	}

	byName := map[string]tools.Tool{}
	for _, tl := range ts {
		byName[tl.Name()] = tl
	}
	// The skill surfaces as a tool that returns its instructions on demand.
	skill, ok := byName["skill_greet"]
	if !ok {
		t.Fatal("skill not exposed as a tool")
	}
	out, _ := skill.Run(context.Background(), "", nil)
	if out != "be warm" {
		t.Errorf("skill tool returned %q, want its instructions", out)
	}
	// The native plugin surfaces as its tool.
	if _, ok := byName["echo"]; !ok {
		t.Error("native plugin tool not exposed")
	}

	// They register into the loop's registry exactly like built-in tools.
	loopReg := tools.NewRegistry(ts...)
	if !loopReg.Has("skill_greet") || !loopReg.Has("echo") {
		t.Error("skills/plugins should register into the native loop registry")
	}
}

// TestParseSkillNameSanitized proves an out-of-spec frontmatter name is slugified
// to the API tool-name charset (so "skill_"+name can never poison a request), and
// that a name with no valid characters is rejected rather than silently emitted.
func TestParseSkillNameSanitized(t *testing.T) {
	// A frontmatter name is exercised through the exported LoadDir path.
	load := func(t *testing.T, name string) ([]skills.Skill, error) {
		t.Helper()
		dir := t.TempDir()
		sd := filepath.Join(dir, "s")
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
		return skills.LoadDir(dir)
	}

	validName := func(n string) bool {
		if len(n) == 0 || len(n) > 64 {
			return false
		}
		for i := 0; i < len(n); i++ {
			c := n[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
				// allowed API tool-name character
			default:
				return false
			}
		}
		return true
	}

	tests := []struct {
		frontmatter string
		want        string // "" means expect a load error
	}{
		{"greet", "greet"},                                  // clean name unchanged
		{"my greet skill", "my-greet-skill"},                // spaces collapse to '-'
		{"héllo wörld", "h-llo-w-rld"},                      // non-ASCII collapses to '-'
		{"a/b:c", "a-b-c"},                                  // punctuation collapses
		{"  spaced  ", "spaced"},                            // leading/trailing trimmed
		{strings.Repeat("x", 200), strings.Repeat("x", 58)}, // truncated to budget (64-6)
	}
	for _, tt := range tests {
		got, err := load(t, tt.frontmatter)
		if tt.want == "" {
			if err == nil {
				t.Errorf("name %q: expected load error, got skill %+v", tt.frontmatter, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("name %q: unexpected error: %v", tt.frontmatter, err)
		}
		if len(got) != 1 || got[0].Name != tt.want {
			t.Fatalf("name %q: got %+v, want name %q", tt.frontmatter, got, tt.want)
		}
		// The derived tool name must satisfy the provider contract, prefix included.
		if toolName := "skill_" + got[0].Name; !validName(toolName) {
			t.Errorf("name %q: tool name %q violates ^[a-zA-Z0-9_-]{1,64}$", tt.frontmatter, toolName)
		}
	}
}

// TestParseSkillAllInvalidNameRejected proves a name with no valid characters is
// rejected — never emitted as an empty or poison tool name.
func TestParseSkillAllInvalidNameRejected(t *testing.T) {
	dir := t.TempDir()
	sd := filepath.Join(dir, "s")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	// Frontmatter name with only non-ASCII/space runes: nothing survives slugify.
	md := "---\nname: 日本 语\ndescription: d\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := skills.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a skill name with no valid characters")
	}
}
