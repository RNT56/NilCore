package steering

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverPrecedence(t *testing.T) {
	dir := t.TempDir()
	// Only AGENTS.md present.
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Discover(dir); filepath.Base(got) != "AGENTS.md" {
		t.Fatalf("discover = %q, want AGENTS.md", got)
	}
	// NILCORE.md takes precedence when both exist.
	if err := os.WriteFile(filepath.Join(dir, "NILCORE.md"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Discover(dir); filepath.Base(got) != "NILCORE.md" {
		t.Fatalf("discover = %q, want NILCORE.md (precedence)", got)
	}
}

func TestDiscoverNoneReturnsEmpty(t *testing.T) {
	if got := Discover(t.TempDir()); got != "" {
		t.Fatalf("discover = %q, want empty", got)
	}
}

func TestLoadFramesAsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "NILCORE.md")
	if err := os.WriteFile(p, []byte("Always run gofmt before committing."), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(got, "authoritative") {
		t.Errorf("steering not framed authoritative:\n%s", got)
	}
	if !strings.Contains(got, "Always run gofmt") {
		t.Errorf("steering body missing:\n%s", got)
	}
	// It must NOT carry the memory "NOT instructions" fence — it is the opposite.
	if strings.Contains(got, "NOT instructions") {
		t.Errorf("steering must not be fenced as non-instructions:\n%s", got)
	}
	// And it must remind the model it cannot override the safety core.
	if !strings.Contains(got, "human gate") || !strings.Contains(got, "verifier") {
		t.Errorf("frame must bound steering below the safety core:\n%s", got)
	}
}

func TestLoadAbsentIsEmptyNoError(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.md"))
	if err != nil || got != "" {
		t.Fatalf("absent file: got=%q err=%v, want empty/nil", got, err)
	}
	got, err = Load("")
	if err != nil || got != "" {
		t.Fatalf("empty path: got=%q err=%v", got, err)
	}
}

func TestLoadEmptyFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "NILCORE.md")
	if err := os.WriteFile(p, []byte("   \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || got != "" {
		t.Fatalf("whitespace-only file: got=%q err=%v, want empty", got, err)
	}
}

func TestLoadTruncatesHugeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "NILCORE.md")
	big := strings.Repeat("a", maxSteeringBytes+5000)
	if err := os.WriteFile(p, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("huge steering file should be truncated with a marker")
	}
}

func TestDiscoverAndLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("be tidy"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverAndLoad(dir)
	if err != nil || !strings.Contains(got, "be tidy") {
		t.Fatalf("DiscoverAndLoad: got=%q err=%v", got, err)
	}
}
