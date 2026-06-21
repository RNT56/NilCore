package ast

import (
	"testing"
)

// A representative C# fixture: a block-scoped namespace, a generic class with an
// attributed async method, an auto-property (`Name { get; set; }`), an expression-bodied
// property (`Area => ...`), a method calling helpers, an interface, a struct, an enum,
// and a record. Decoys in a comment/string and control keywords (`if`, `foreach`,
// `new`, `nameof`) must NOT read as calls.
const csSample = `using System;

namespace App {

    // a comment with decoy() that must not register
    public class Box<T> {
        public string Name { get; set; }
        public int Area => Compute(0);

        [Obsolete]
        public async Task<int> MapAsync<R>(Func<T, R> f) {
            /* block decoy() ignored */
            var s = "string decoy() ignored";
            return Transform(f);
        }

        private int Compute(int seed) {
            if (seed > 0) {
                return Helper(new object());
            }
            return seed;
        }
    }

    public interface IShape {
        double Area();
    }

    public struct Point {
        public int X;
    }

    public enum Color { Red, Green, Blue }

    public record Pair(int A, int B);
}
`

func TestCSharpSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "Sample.cs", csSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}

	tests := []struct {
		name string
		kind Kind
		recv string
	}{
		{"App", KindType, ""},
		{"Box", KindType, ""},
		{"Name", KindMethod, "Box"},     // auto-property
		{"Area", KindMethod, "Box"},     // expression-bodied property
		{"MapAsync", KindMethod, "Box"}, // generic+attributed async method
		{"Compute", KindMethod, "Box"},
		{"IShape", KindType, ""},
		{"Point", KindType, ""},
		{"Color", KindType, ""},
		{"Pair", KindType, ""},
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
	}
}

func TestCSharpSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "Sample.cs", csSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// MapAsync has a multi-line body.
	if s := got["MapAsync"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("MapAsync span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
	// The auto-property Name is a single-line member (no nested block to overrun).
	if s := got["Name"]; s.Span.StartLine != s.Span.EndLine {
		t.Errorf("auto-property Name span = %d-%d, want a single line", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestCSharpReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "Sample.cs", csSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"Transform", "Helper", "Compute"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys, the methods' own names, and control keywords must not register.
	for _, bad := range []string{"decoy", "MapAsync", "if", "new"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestCSharpCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "Sample.cs", csSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["MapAsync"], "Transform") {
		t.Errorf("MapAsync should call Transform; got %+v", calls["MapAsync"])
	}
	if !contains(calls["Compute"], "Helper") {
		t.Errorf("Compute should call Helper; got %+v", calls["Compute"])
	}
	for _, fn := range []string{"MapAsync", "Compute"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}

// A file-scoped namespace (`namespace Foo;`) records the namespace but its types live at
// file scope.
func TestCSharpFileScopedNamespace(t *testing.T) {
	src := "namespace Lib;\n\npublic class Service {\n    public void Go() { Run(); }\n}\n"
	syms, err := Symbols(writeSrc(t, "fs.cs", src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	if s, ok := got["Lib"]; !ok || s.Kind != KindType {
		t.Errorf("file-scoped namespace Lib not recorded: %+v", syms)
	}
	if s, ok := got["Service"]; !ok || s.Kind != KindType {
		t.Errorf("class Service not extracted under file-scoped namespace: %+v", syms)
	}
	if s, ok := got["Go"]; !ok || s.Kind != KindMethod || s.Recv != "Service" {
		t.Errorf("method Go not extracted with recv Service: %+v", syms)
	}
}
