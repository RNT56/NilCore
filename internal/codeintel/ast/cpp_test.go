package ast

import (
	"testing"
)

// A representative C++ fixture: a namespace, a class with an in-class method and an
// inline constructor, an out-of-line method definition (`Widget::render`), an out-of-line
// constructor and destructor, a templated free function, an operator overload, an enum
// class, and a free function. Decoys in a comment/string and control keywords (`if`,
// `for`, `new`, `static_cast`) must NOT read as calls.
const cppSample = `#include <vector>

namespace gfx {

// a comment with decoy() that must not register
class Widget {
public:
    Widget(int w) : width_(w) {}
    int width() const { return width_; }
    bool operator==(const Widget &o) const { return width_ == o.width_; }
private:
    int width_;
};

void Widget::render() {
    /* block decoy() ignored */
    const char *s = "string decoy() ignored";
    draw(width());
}

Widget::~Widget() {
    cleanup();
}

template <typename T>
T max_of(T a, T b) {
    return a > b ? a : b;
}

enum class Mode { On, Off };

int run(int n) {
    if (n > 0) {
        return compute(new Widget(n));
    }
    return static_cast<int>(0);
}

} // namespace gfx
`

func TestCppSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.cpp", cppSample))
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
		{"gfx", KindType, ""},
		{"width", KindMethod, "Widget"},      // in-class method
		{"render", KindMethod, "Widget"},     // out-of-line: Widget::render
		{"operator==", KindMethod, "Widget"}, // in-class operator overload
		{"max_of", KindFunc, ""},             // templated free function
		{"Mode", KindType, ""},               // enum class
		{"run", KindFunc, ""},                // free function
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

	// The class type `Widget` and the constructor `Widget(...)` share a name; assert both
	// exist as distinct symbols (a name-keyed map would hide one).
	var sawClass bool
	for _, s := range syms {
		if s.Name == "Widget" && s.Kind == KindType && s.Recv == "" {
			sawClass = true
		}
	}
	if !sawClass {
		t.Errorf("class type Widget not captured: %+v", syms)
	}

	// The out-of-line destructor `Widget::~Widget()` is a method on Widget named ~Widget.
	var sawDtor bool
	for _, s := range syms {
		if s.Name == "~Widget" && s.Kind == KindMethod && s.Recv == "Widget" {
			sawDtor = true
		}
	}
	if !sawDtor {
		t.Errorf("out-of-line destructor Widget::~Widget not captured with recv Widget: %+v", syms)
	}

	// The in-class constructor `Widget(int w)` is a method on Widget named Widget.
	var sawCtor bool
	for _, s := range syms {
		if s.Name == "Widget" && s.Kind == KindMethod && s.Recv == "Widget" {
			sawCtor = true
		}
	}
	if !sawCtor {
		t.Errorf("in-class constructor Widget(...) not captured as method with recv Widget: %+v", syms)
	}
}

func TestCppOutOfLineSpan(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.cpp", cppSample))
	if err != nil {
		t.Fatal(err)
	}
	var render Symbol
	for _, s := range syms {
		if s.Name == "render" && s.Kind == KindMethod {
			render = s
		}
	}
	if render.Span.EndLine <= render.Span.StartLine {
		t.Errorf("out-of-line render span = %d-%d, want a multi-line body", render.Span.StartLine, render.Span.EndLine)
	}
}

func TestCppReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.cpp", cppSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"draw", "width", "cleanup", "compute"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys, the methods' own names, and control/cast keywords must not register.
	for _, bad := range []string{"decoy", "render", "run", "if", "for", "new", "static_cast"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestCppCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.cpp", cppSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["render"], "draw") || !contains(calls["render"], "width") {
		t.Errorf("render should call draw and width; got %+v", calls["render"])
	}
	if !contains(calls["run"], "compute") {
		t.Errorf("run should call compute; got %+v", calls["run"])
	}
	if !contains(calls["~Widget"], "cleanup") {
		t.Errorf("~Widget should call cleanup; got %+v", calls["~Widget"])
	}
}

// .cc/.cxx/.hpp/.hh/.hxx all dispatch to the C++ backend.
func TestCppExtensions(t *testing.T) {
	for _, name := range []string{"a.cc", "a.cxx", "a.hpp", "a.hh", "a.hxx"} {
		src := "class A {\npublic:\n    void go() {\n        step();\n    }\n};\n"
		syms, err := Symbols(writeSrc(t, name, src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sawGo bool
		for _, s := range syms {
			if s.Name == "go" && s.Kind == KindMethod && s.Recv == "A" {
				sawGo = true
			}
		}
		if !sawGo {
			t.Errorf("%s: method go on A not extracted: %+v", name, syms)
		}
	}
}
