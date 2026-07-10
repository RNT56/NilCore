package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/skills"
	"nilcore/internal/tools"
)

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

func TestAsToolsExposesSkills(t *testing.T) {
	reg := skills.New(
		[]skills.Skill{{Name: "greet", Description: "greet", Instructions: "be warm"}},
	)
	ts := reg.AsTools()
	if len(ts) != 1 {
		t.Fatalf("AsTools = %d, want 1", len(ts))
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

	// It registers into the loop's registry exactly like a built-in tool.
	loopReg := tools.NewRegistry(ts...)
	if !loopReg.Has("skill_greet") {
		t.Error("skill should register into the native loop registry")
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

// writeSkillFile writes a SKILL.md with the given body under dir/<sub>/SKILL.md.
func writeSkillFile(t *testing.T, dir, sub, md string) {
	t.Helper()
	sd := filepath.Join(dir, sub)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestParseSkillAllInvalidNameSkipped proves a name with no valid characters is
// SKIPPED (never emitted as an empty or poison tool name) AND does not zero out a
// sibling valid skill — loading is additive, a bad skill is a warning, not a failure.
func TestParseSkillAllInvalidNameSkipped(t *testing.T) {
	dir := t.TempDir()
	// Frontmatter name with only non-ASCII/space runes: nothing survives slugify, so
	// this skill must be dropped rather than poisoning the tool name.
	writeSkillFile(t, dir, "bad", "---\nname: 日本 语\ndescription: d\n---\nbody\n")
	writeSkillFile(t, dir, "good", "---\nname: greet\ndescription: d\n---\nbody\n")

	loaded, err := skills.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir must skip the bad skill, not error: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Name != "greet" {
		t.Fatalf("want only the valid skill loaded, got %+v", loaded)
	}
}

// TestLoadDirSkipsMalformedContinuesRest is the headline for fix 1: a directory with
// one malformed SKILL.md and two valid ones must load the TWO valid skills — one bad
// file can never zero out the rest (which the cmd loader would otherwise discard
// wholesale on a LoadDir error).
func TestLoadDirSkipsMalformedContinuesRest(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "alpha", "---\nname: alpha\ndescription: a\n---\nbody a\n")
	writeSkillFile(t, dir, "broken", "no frontmatter at all — this file is malformed\n")
	writeSkillFile(t, dir, "beta", "---\nname: beta\ndescription: b\n---\nbody b\n")

	loaded, err := skills.LoadDir(dir)
	if err != nil {
		t.Fatalf("one malformed skill must not fail the whole load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("want 2 valid skills loaded (malformed skipped), got %d: %+v", len(loaded), loaded)
	}
	got := map[string]bool{}
	for _, s := range loaded {
		got[s.Name] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Fatalf("expected both valid skills (alpha, beta), got %+v", loaded)
	}
}

// TestLoadDirWarnsOnNameCollision proves two skills whose frontmatter names sanitize
// to the SAME tool name do not both load (which would silently shadow in the
// registry): the first wins and the collision is dropped, not fatal.
func TestLoadDirWarnsOnNameCollision(t *testing.T) {
	dir := t.TempDir()
	// "my greet" and "my/greet" both sanitize to "my-greet".
	writeSkillFile(t, dir, "a-first", "---\nname: my greet\ndescription: a\n---\nbody a\n")
	writeSkillFile(t, dir, "b-second", "---\nname: my/greet\ndescription: b\n---\nbody b\n")

	loaded, err := skills.LoadDir(dir)
	if err != nil {
		t.Fatalf("a name collision must not fail the load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("colliding tool names must not both load, got %d: %+v", len(loaded), loaded)
	}
	if loaded[0].Name != "my-greet" {
		t.Fatalf("collision survivor name = %q, want %q", loaded[0].Name, "my-greet")
	}
}
