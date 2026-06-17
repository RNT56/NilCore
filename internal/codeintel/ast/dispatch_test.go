package ast

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The seam must dispatch by extension and skip — not error — on anything it has no
// backend for, so a broadened index walk over a mixed-language tree degrades cleanly.
func TestUnsupportedExtensionIsSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(p, []byte("def looks_like_python(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	syms, err := Symbols(p)
	if err != nil || syms != nil {
		t.Errorf("Symbols(.txt) = (%+v, %v), want (nil, nil)", syms, err)
	}
	refs, err := References(p)
	if err != nil || refs != nil {
		t.Errorf("References(.txt) = (%+v, %v), want (nil, nil)", refs, err)
	}
	calls, err := Calls(p)
	if err != nil || calls != nil {
		t.Errorf("Calls(.txt) = (%+v, %v), want (nil, nil)", calls, err)
	}
}

// A missing supported-extension file is a real error (unlike an unsupported ext).
func TestSupportedExtensionMissingFileErrors(t *testing.T) {
	if _, err := Symbols(filepath.Join(t.TempDir(), "gone.go")); err == nil {
		t.Error("Symbols(missing .go) should error")
	}
}

// Dispatch is case-insensitive on the extension.
func TestUpperCaseExtensionDispatches(t *testing.T) {
	p := filepath.Join(t.TempDir(), "Mod.PY")
	if err := os.WriteFile(p, []byte("def f():\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syms, err := Symbols(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Name != "f" {
		t.Errorf("uppercase .PY did not dispatch to the python backend: %+v", syms)
	}
}

func TestSupportedExtensions(t *testing.T) {
	got := SupportedExtensions()
	sort.Strings(got)
	want := []string{".go", ".py"}
	if len(got) != len(want) {
		t.Fatalf("SupportedExtensions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SupportedExtensions() = %v, want %v", got, want)
		}
	}
}
