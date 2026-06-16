package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeSkill(t *testing.T, path, name, version, body string) {
	t.Helper()
	content := "---\nname: " + name + "\n"
	if version != "" {
		content += "version: " + version + "\n"
	}
	content += "description: a test skill\n---\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallSkillCopiesAndVerifies(t *testing.T) {
	src := filepath.Join(t.TempDir(), "SKILL.md")
	writeSkill(t, src, "deploy-helper", "1.2.0", "Step 1. Do the thing.")
	skillsDir := t.TempDir()

	if err := InstallSkill(Entry{Name: "deploy-helper", Kind: KindSkill, Version: "1.2.0", Source: src}, skillsDir); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := Installed(skillsDir)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if len(got) != 1 || got[0].Name != "deploy-helper" || got[0].Version != "1.2.0" {
		t.Fatalf("installed = %+v", got)
	}
}

func TestInstallSkillRollsBackUnparseable(t *testing.T) {
	src := filepath.Join(t.TempDir(), "SKILL.md")
	// No frontmatter -> parseSkill fails -> install must roll back.
	if err := os.WriteFile(src, []byte("just some text, no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsDir := t.TempDir()
	if err := InstallSkill(Entry{Name: "bad", Kind: KindSkill, Source: src}, skillsDir); err == nil {
		t.Fatal("want error for an unparseable skill")
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "bad")); !os.IsNotExist(err) {
		t.Error("a failed install must leave no skill directory behind (rollback)")
	}
}

func TestInstallSkillRejectsNonSkillKind(t *testing.T) {
	if err := InstallSkill(Entry{Name: "x", Kind: KindMCP, Source: "y"}, t.TempDir()); err == nil {
		t.Fatal("mcp install is a follow-up; must reject here")
	}
}

func TestLoadManifestAndFilter(t *testing.T) {
	dir := t.TempDir()
	mpath := filepath.Join(dir, "manifest.json")
	m := Manifest{Entries: []Entry{
		{Name: "s1", Kind: KindSkill, Version: "1.0.0", Source: "/a/SKILL.md"},
		{Name: "srv", Kind: KindMCP, Version: "0.1.0", Source: "/b"},
	}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(mpath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(mpath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d", len(got.Entries))
	}
	if sk := got.Skills(); len(sk) != 1 || sk[0].Name != "s1" {
		t.Fatalf("skills filter = %+v", sk)
	}
}

func TestLoadManifestMissingIsEmpty(t *testing.T) {
	got, err := LoadManifest(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(got.Entries) != 0 {
		t.Fatalf("missing manifest: got=%+v err=%v", got, err)
	}
}
