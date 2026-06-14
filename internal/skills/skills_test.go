package skills_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
