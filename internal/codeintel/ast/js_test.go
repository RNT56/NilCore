package ast

import (
	"os"
	"path/filepath"
	"testing"
)

// A representative TypeScript fixture exercising every JS/TS shape the backend models:
// an exported free function, an exported arrow-function binding, a function-expression
// binding, a class with a constructor + method + member call, bare/dotted call sites,
// and decoys inside a line comment, a block comment, a string, and a template literal
// that must NOT register as calls.
const tsSample = `import { fmt } from "./fmt";

// a comment with decoy() that must not register
export function greet(name: string): string {
  return fmt(name);
}

export const sum = (a: number, b: number): number => {
  return a + b;
};

const build = function (n) {
  return new Calc(n);
};

class Calc {
  constructor(n: number) {
    this.n = n;
  }

  double(): number {
    /* block decoy() ignored */
    const label = ` + "`x is ${notACall()}`" + `;
    return this.helper(sum(this.n, this.n));
  }
}
`

func writeSrc(t *testing.T, name, src string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJSSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.ts", tsSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}

	tests := []struct {
		name  string
		kind  Kind
		recv  string
		start int
	}{
		{"greet", KindFunc, "", 4},
		{"sum", KindFunc, "", 8},
		{"build", KindFunc, "", 12},
		{"Calc", KindType, "", 16},
		{"constructor", KindMethod, "Calc", 17},
		{"double", KindMethod, "Calc", 21},
	}
	for _, tc := range tests {
		s, ok := got[tc.name]
		if !ok {
			t.Errorf("%s: not extracted", tc.name)
			continue
		}
		if s.Kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, s.Kind, tc.kind)
		}
		if s.Recv != tc.recv {
			t.Errorf("%s: recv = %q, want %q", tc.name, s.Recv, tc.recv)
		}
		if s.Span.StartLine != tc.start {
			t.Errorf("%s: start line = %d, want %d", tc.name, s.Span.StartLine, tc.start)
		}
	}
}

func TestJSSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.ts", tsSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// greet is a brace block from its header (4) to its matching close brace (6).
	if s := got["greet"]; s.Span.StartLine != 4 || s.Span.EndLine != 6 {
		t.Errorf("greet span = %d-%d, want 4-6", s.Span.StartLine, s.Span.EndLine)
	}
	// The arrow binding's body runs 8-10.
	if s := got["sum"]; s.Span.StartLine != 8 || s.Span.EndLine != 10 {
		t.Errorf("sum span = %d-%d, want 8-10", s.Span.StartLine, s.Span.EndLine)
	}
	// Calc spans its whole body through double's closing brace.
	if s := got["Calc"]; s.Span.StartLine != 16 || s.Span.EndLine < 26 {
		t.Errorf("Calc span = %d-%d, want 16 through at least 26", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestJSReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.ts", tsSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"fmt", "sum", "helper", "Calc"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys in a line comment, block comment, string, and template literal must not
	// register; nor must a header's own name read as a self-call.
	for _, bad := range []string{"decoy", "notACall", "greet", "double"} {
		if names[bad] {
			t.Errorf("decoy/self-call %q leaked into references: %+v", bad, refs)
		}
	}
	// `this.helper(...)` records the trailing simple name "helper".
	var sawHelper bool
	for _, r := range refs {
		if r.Name == "helper" {
			sawHelper = true
		}
	}
	if !sawHelper {
		t.Errorf("dotted call this.helper() should record trailing name helper; got %+v", refs)
	}
}

func TestJSCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.ts", tsSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["greet"], "fmt") {
		t.Errorf("greet should call fmt; got %+v", calls["greet"])
	}
	if !contains(calls["double"], "sum") || !contains(calls["double"], "helper") {
		t.Errorf("double should call sum and helper; got %+v", calls["double"])
	}
	if !contains(calls["build"], "Calc") {
		t.Errorf("build should construct Calc; got %+v", calls["build"])
	}
	// sum() calls nothing but must still be a key (mirrors the Go backend).
	if _, ok := calls["sum"]; !ok {
		t.Errorf("sum should be a key even with no calls; got keys %v", keys(calls))
	}
}

// A single-parameter arrow without parentheses (`const f = x => {`) must still be
// captured, and `export default function` must not shift the name capture.
func TestJSArrowAndExportDefaultVariants(t *testing.T) {
	src := "const square = x => {\n  return x * x;\n};\n\nexport default function main() {\n  return square(2);\n}\n"
	syms, err := Symbols(writeSrc(t, "v.js", src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	if s, ok := got["square"]; !ok || s.Kind != KindFunc {
		t.Errorf("paren-less arrow `x =>` not captured as func: %+v", syms)
	}
	if s, ok := got["main"]; !ok || s.Kind != KindFunc {
		t.Errorf("`export default function main` not captured: %+v", syms)
	}
}
