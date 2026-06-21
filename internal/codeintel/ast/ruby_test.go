package ast

import (
	"testing"
)

// A representative Ruby fixture exercising the shapes the backend models: a top-level
// `def`, a module nesting a class, instance methods, a singleton method `def self.name`
// (a class method, still on the enclosing type), a method whose body uses `do...end` and
// `if...end` nesting (so the matching `end` tracking is exercised), method calls in both
// `name(...)` and `recv.method` forms, and a one-line `def x; ... ; end`. Decoys in `#`
// comments, `=begin/=end` block comments, and strings must NOT register; control keywords
// must not read as calls.
const rubySample = `# a comment with decoy() that must not register
=begin
block comment with decoy2() ignored
=end

def top_level(n)
  helper(n)
end

module Geometry
  class Box
    def initialize(value)
      @value = value
    end

    def greet
      format(@value)
    end

    def loop_it(items)
      items.each do |item|
        if item.valid?
          process(item)
        end
      end
    end

    def self.make(value)
      Box.new(value)
    end

    def one_liner; render; end

    private

    def format(x)
      s = "string decoy() ignored"
      x.to_s
    end

    def render
      draw()
    end
  end

  module Helpers
    def self.assist
      compute()
    end
  end
end
`

func TestRubySymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.rb", rubySample))
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
		{"top_level", KindFunc, ""},
		{"Geometry", KindType, ""},
		{"Box", KindType, ""},
		{"initialize", KindMethod, "Box"},
		{"greet", KindMethod, "Box"},
		{"loop_it", KindMethod, "Box"},
		{"make", KindMethod, "Box"},      // def self.make -> singleton method, Recv Box
		{"one_liner", KindMethod, "Box"}, // single-line def ...; end
		{"format", KindMethod, "Box"},
		{"render", KindMethod, "Box"},
		{"Helpers", KindType, ""},
		{"assist", KindMethod, "Helpers"}, // def self.assist inside a module
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

func TestRubySpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.rb", rubySample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// loop_it nests a do...end and an if...end; its span must cover the whole body.
	if s := got["loop_it"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("loop_it span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
	// Box spans through every member, well past its header.
	if s := got["Box"]; s.Span.EndLine < s.Span.StartLine+20 {
		t.Errorf("Box type span = %d-%d, want it to cover the whole class body", s.Span.StartLine, s.Span.EndLine)
	}
	// one_liner is a single-line def.
	if s := got["one_liner"]; s.Span.StartLine != s.Span.EndLine {
		t.Errorf("one_liner span = %d-%d, want a single line", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestRubyReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.rb", rubySample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// helper(n); format(@value); items.each (.each); process(item); compute; draw.
	for _, want := range []string{"helper", "format", "each", "process", "compute", "draw"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys, control keywords, and bare-word self-names must not register.
	for _, bad := range []string{"decoy", "decoy2", "if", "end", "do"} {
		if names[bad] {
			t.Errorf("decoy/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestRubyCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.rb", rubySample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["top_level"], "helper") {
		t.Errorf("top_level should call helper; got %+v", calls["top_level"])
	}
	if !contains(calls["greet"], "format") {
		t.Errorf("greet should call format; got %+v", calls["greet"])
	}
	if !contains(calls["loop_it"], "process") {
		t.Errorf("loop_it should call process; got %+v", calls["loop_it"])
	}
	if !contains(calls["assist"], "compute") {
		t.Errorf("assist should call compute; got %+v", calls["assist"])
	}
	for _, fn := range []string{"top_level", "greet", "loop_it", "assist"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}
