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
		// Tier-1 brace languages (Phase 13).
		{"a.java", "class J {\n    void m() {\n    }\n}\n", "J"},
		{"a.c", "int cf(void) {\n    return 0;\n}\n", "cf"},
		{"a.h", "struct S {\n    int x;\n};\n", "S"},
		{"a.cc", "int cc() {\n    return 0;\n}\n", "cc"},
		{"a.cpp", "int cpp() {\n    return 0;\n}\n", "cpp"},
		{"a.cxx", "int cxx() {\n    return 0;\n}\n", "cxx"},
		{"a.hpp", "class H {\n};\n", "H"},
		{"a.hh", "class HH {\n};\n", "HH"},
		{"a.hxx", "class HX {\n};\n", "HX"},
		{"a.cs", "class CS {\n    void M() {\n    }\n}\n", "CS"},
		// Tier-2 languages (Phase 13).
		{"a.rb", "def rb\n  1\nend\n", "rb"},
		{"a.php", "<?php\nfunction pf() {\n    return 1;\n}\n", "pf"},
		{"a.kt", "fun kf() {\n    return\n}\n", "kf"},
		{"a.kts", "fun ks() {\n    return\n}\n", "ks"},
		{"a.swift", "func sf() {\n    return\n}\n", "sf"},
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
	// Rust, the Tier-1 brace languages (Java, C, C++, C#), and the Tier-2 languages
	// (Ruby, PHP, Kotlin, Swift). Kept exhaustive so a forgotten registration (or an
	// accidental drop) fails loudly.
	want := []string{
		".c", ".cc", ".cjs", ".cpp", ".cs", ".cxx", ".go", ".h", ".hh", ".hpp",
		".hxx", ".java", ".js", ".jsx", ".kt", ".kts", ".mjs", ".php", ".py",
		".rb", ".rs", ".swift", ".ts", ".tsx",
	}
	if len(got) != len(want) {
		t.Fatalf("SupportedExtensions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SupportedExtensions() = %v, want %v", got, want)
		}
	}
}
