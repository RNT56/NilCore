package ast

import (
	"testing"
)

// A representative Swift fixture exercising the shapes the backend models: a top-level
// function, a struct with an initializer and methods (one generic, one `mutating`, one
// with external/internal argument labels), a class, a protocol, an enum, an `actor`, and
// an `extension` adding a method (receiver from the extension header). An `@objc`
// attribute and decoys in comments/strings must NOT register as calls; control keywords
// (`if`, `guard`, `for`) must not read as calls.
const swiftSample = `import Foundation

// a comment with decoy() that must not register
func topLevel(_ n: Int) -> Int {
    return helper(n)
}

struct Box<T> {
    let value: T

    init(value: T) {
        self.value = value
    }

    func map<R>(_ f: (T) -> R) -> R {
        /* block decoy() ignored */
        let s = "string decoy() ignored"
        return transform(f(value))
    }

    mutating func reset(to newValue: T) {
        store(newValue)
    }

    private func transform<R>(_ x: R) -> R {
        return x
    }
}

class Widget {
    @objc func tap() {
        handle()
    }
}

protocol Shape {
    func area() -> Double
}

enum Color {
    case red, green, blue
}

actor Counter {
    func increment() {
        bump()
    }
}

extension Box {
    func doubled() -> Int {
        return compute(0)
    }
}
`

func TestSwiftSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.swift", swiftSample))
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
		{"topLevel", KindFunc, ""},
		{"Box", KindType, ""},
		{"map", KindMethod, "Box"},
		{"reset", KindMethod, "Box"},
		{"transform", KindMethod, "Box"},
		{"Widget", KindType, ""},
		{"tap", KindMethod, "Widget"},
		{"Shape", KindType, ""},
		{"Color", KindType, ""},
		{"Counter", KindType, ""},
		{"increment", KindMethod, "Counter"},
		{"doubled", KindMethod, "Box"}, // added in `extension Box`
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

	// The initializer `init` is a method on Box.
	var sawInit bool
	for _, s := range syms {
		if s.Name == "init" && s.Kind == KindMethod && s.Recv == "Box" {
			sawInit = true
		}
	}
	if !sawInit {
		t.Errorf("init not captured as a method with recv Box: %+v", syms)
	}
}

func TestSwiftSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.swift", swiftSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	if s := got["map"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("map span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
	if s := got["Box"]; s.Span.EndLine < s.Span.StartLine+10 {
		t.Errorf("Box type span = %d-%d, want it to cover the whole struct body", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestSwiftReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.swift", swiftSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"transform", "store", "handle", "bump", "compute", "helper"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	for _, bad := range []string{"decoy", "map", "if", "guard", "for"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestSwiftCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.swift", swiftSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["map"], "transform") {
		t.Errorf("map should call transform; got %+v", calls["map"])
	}
	if !contains(calls["doubled"], "compute") {
		t.Errorf("doubled should call compute; got %+v", calls["doubled"])
	}
	if !contains(calls["increment"], "bump") {
		t.Errorf("increment should call bump; got %+v", calls["increment"])
	}
	for _, fn := range []string{"map", "doubled", "increment"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}
