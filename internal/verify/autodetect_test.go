package verify

import (
	"os"
	"path/filepath"
	"testing"
)

// seed writes name=content files into a fresh temp dir and returns its path.
func seed(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "makefile with verify target",
			files: map[string]string{"Makefile": "build:\n\tgo build ./...\nverify: build\n\tgo test ./...\n"},
			want:  "make verify",
		},
		{
			name:  "makefile verify target with leading whitespace",
			files: map[string]string{"Makefile": "  verify:\n\techo ok\n"},
			want:  "make verify",
		},
		{
			name:  "lowercase makefile with verify target",
			files: map[string]string{"makefile": "verify:\n\ttrue\n"},
			want:  "make verify",
		},
		{
			name:  "makefile without verify target falls through to go",
			files: map[string]string{"Makefile": "build:\n\tgo build ./...\n", "go.mod": "module x\n"},
			want:  "go build ./... && go test ./...",
		},
		{
			name:  "makefile with similar but non-matching target falls through",
			files: map[string]string{"Makefile": "verify-fast:\n\ttrue\n"},
			want:  "true",
		},
		{
			name:  "go module",
			files: map[string]string{"go.mod": "module example.com/x\n\ngo 1.25\n"},
			want:  "go build ./... && go test ./...",
		},
		{
			name:  "node package",
			files: map[string]string{"package.json": `{"name":"x"}`},
			want:  "npm test",
		},
		{
			name:  "rust crate",
			files: map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"},
			want:  "cargo test",
		},
		{
			name:  "python pyproject",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n"},
			want:  "pytest",
		},
		{
			name:  "python setup.py",
			files: map[string]string{"setup.py": "from setuptools import setup\nsetup()\n"},
			want:  "pytest",
		},
		{
			name:  "makefile verify beats go.mod",
			files: map[string]string{"Makefile": "verify:\n\ttrue\n", "go.mod": "module x\n"},
			want:  "make verify",
		},
		{
			name:  "go beats package.json",
			files: map[string]string{"go.mod": "module x\n", "package.json": "{}"},
			want:  "go build ./... && go test ./...",
		},
		{
			name:  "empty dir falls back to true",
			files: nil,
			want:  "true",
		},
		{
			name:  "unrecognized files fall back to true",
			files: map[string]string{"README.md": "# hi\n", "main.c": "int main(){}"},
			want:  "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := seed(t, tt.files)
			if got := Detect(dir); got != tt.want {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectUnknownDirFallback(t *testing.T) {
	// A path that does not exist must not error or fail-detect; it falls back.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if got := Detect(missing); got != "true" {
		t.Errorf("Detect(missing dir) = %q, want %q", got, "true")
	}
}

func TestDetectOrOverride(t *testing.T) {
	goDir := seed(t, map[string]string{"go.mod": "module x\n"})

	tests := []struct {
		name     string
		dir      string
		override string
		want     string
	}{
		{
			name:     "non-empty override wins over detection",
			dir:      goDir,
			override: "make check",
			want:     "make check",
		},
		{
			name:     "empty override falls back to detection",
			dir:      goDir,
			override: "",
			want:     "go build ./... && go test ./...",
		},
		{
			name:     "whitespace-only override falls back to detection",
			dir:      goDir,
			override: "   ",
			want:     "go build ./... && go test ./...",
		},
		{
			name:     "override wins even for unknown dir",
			dir:      t.TempDir(),
			override: "custom-cmd",
			want:     "custom-cmd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectOrOverride(tt.dir, tt.override); got != tt.want {
				t.Errorf("DetectOrOverride() = %q, want %q", got, tt.want)
			}
		})
	}
}
