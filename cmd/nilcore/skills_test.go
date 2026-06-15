package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadSkillTools discovers SKILL.md files and exposes each as a skill_<name> tool
// that returns its instructions, registered alongside the built-ins.
func TestLoadSkillTools(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "deploy")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: deploy\ndescription: how to ship a release\n---\n1. tag\n2. push\n"
	if err := os.WriteFile(filepath.Join(sub, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	tls := loadSkillTools(dir)
	if len(tls) != 1 {
		t.Fatalf("loaded %d skill tools, want 1", len(tls))
	}
	if tls[0].Name() != "skill_deploy" {
		t.Errorf("tool name = %q, want skill_deploy", tls[0].Name())
	}
	out, err := tls[0].Run(context.Background(), dir, json.RawMessage(`{}`))
	if err != nil || out == "" {
		t.Fatalf("skill run = %q, %v", out, err)
	}
	if !strings.Contains(out, "tag") {
		t.Errorf("skill instructions missing body: %q", out)
	}

	// An absent directory degrades to no skills, never an error.
	if got := loadSkillTools(filepath.Join(dir, "nope")); got != nil {
		t.Errorf("absent dir must yield no tools, got %d", len(got))
	}
	if got := loadSkillTools(""); got != nil {
		t.Errorf("empty dir must yield no tools, got %d", len(got))
	}
}

// loopTools registers discovered skills alongside the built-in tools.
func TestLoopToolsIncludesBuiltins(t *testing.T) {
	r := loopTools()
	for _, name := range []string{"read", "write", "edit", "search", "git"} {
		if !r.Has(name) {
			t.Errorf("loopTools missing built-in %q", name)
		}
	}
}
