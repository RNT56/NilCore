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

// The JS/TS family and Rust extensions must all dispatch to their backend (not be
// treated as unsupported). We assert a representative source under each suffix yields
// its expected top-level symbol.
func TestNewExtensionsDispatch(t *testing.T) {
	cases := []struct {
		file string
		src  string
		want string
	}{
		{"a.ts", "export function f() {\n  return 1;\n}\n", "f"},
		{"a.tsx", "function Widget() {\n  return null;\n}\n", "Widget"},
		{"a.js", "const g = () => {\n  return 2;\n};\n", "g"},
		{"a.jsx", "function App() {\n  return null;\n}\n", "App"},
		{"a.mjs", "export function m() {\n  return 3;\n}\n", "m"},
		{"a.cjs", "function c() {\n  return 4;\n}\n", "c"},
		{"a.rs", "fn r() -> i32 {\n    0\n}\n", "r"},
	}
	for _, tc := range cases {
		p := filepath.Join(t.TempDir(), tc.file)
		if err := os.WriteFile(p, []byte(tc.src), 0o644); err != nil {
			t.Fatal(err)
		}
		syms, err := Symbols(p)
		if err != nil {
			t.Errorf("%s: Symbols error: %v", tc.file, err)
			continue
		}
		var found bool
		for _, s := range syms {
			if s.Name == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: did not dispatch to a backend (want symbol %q, got %+v)", tc.file, tc.want, syms)
		}
	}
}

func TestSupportedExtensions(t *testing.T) {
	got := SupportedExtensions()
	sort.Strings(got)
	// The full set now spans every registered backend: Go, Python, the JS/TS family,
	// and Rust. Kept exhaustive so a forgotten registration (or an accidental drop)
	// fails loudly.
	want := []string{".cjs", ".go", ".js", ".jsx", ".mjs", ".py", ".rs", ".ts", ".tsx"}
	if len(got) != len(want) {
		t.Fatalf("SupportedExtensions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SupportedExtensions() = %v, want %v", got, want)
		}
	}
}
