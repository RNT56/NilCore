package ast

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `package mathx

type Calc struct{ n int }

const Pi = 3

var Tau = 6

func Add(a, b int) int { return a + b }

func (c *Calc) Double() int { return Add(c.n, c.n) }
`

func writeSample(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mathx.go")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSymbols(t *testing.T) {
	syms, err := Symbols(writeSample(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	if got["Add"].Kind != KindFunc {
		t.Errorf("Add kind = %q", got["Add"].Kind)
	}
	if got["Calc"].Kind != KindType {
		t.Errorf("Calc kind = %q", got["Calc"].Kind)
	}
	if got["Double"].Kind != KindMethod || got["Double"].Recv != "Calc" {
		t.Errorf("Double = %+v, want method on Calc", got["Double"])
	}
	if got["Pi"].Kind != KindConst || got["Tau"].Kind != KindVar {
		t.Errorf("Pi=%q Tau=%q", got["Pi"].Kind, got["Tau"].Kind)
	}
	// Span accuracy: Add is declared on line 9 of the sample.
	if got["Add"].Span.StartLine != 9 {
		t.Errorf("Add span start = %d, want 9", got["Add"].Span.StartLine)
	}
}

func TestReferences(t *testing.T) {
	refs, err := References(writeSample(t))
	if err != nil {
		t.Fatal(err)
	}
	var sawAdd bool
	for _, r := range refs {
		if r.Name == "Add" && r.Span.StartLine == 11 {
			sawAdd = true
		}
	}
	if !sawAdd {
		t.Errorf("expected a reference to Add at line 11; got %+v", refs)
	}
}

// TestCallsSameNamedCallersMerge is the regression for the last-writer-wins bug:
// two functions/methods that share a bare name must have their callee lists merged
// (appended), not overwritten — otherwise the first declaration's outgoing calls
// are silently lost before they reach the call graph.
func TestCallsSameNamedCallersMerge(t *testing.T) {
	src := `package p

type A struct{}
type B struct{}

func (A) Validate() { alpha() }
func (B) Validate() { beta() }

func alpha() {}
func beta()  {}
`
	p := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	calls, err := Calls(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range calls["Validate"] {
		got[c] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("Validate callees = %v, want both alpha and beta (merged, not overwritten)", calls["Validate"])
	}
}

func TestSymbolsParseError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.go")
	if err := os.WriteFile(p, []byte("package x\nfunc ("), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Symbols(p); err == nil {
		t.Error("expected a parse error")
	}
}
