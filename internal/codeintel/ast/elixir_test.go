package ast

import "testing"

// An Elixir fixture exercising: `defmodule Name do` (a module, dotted alias), `def`/`defp`
// (public/private functions with the module as Recv), a `defmacro`, nesting via `if`/`case`/
// `do`/`end`, a one-line `def x, do: ...` inline form, and calls in `name(...)` and
// `Module.func(...)` forms. Decoys in `#` comments and strings must NOT register.
const elixirSample = `# a comment with decoy() that must not register

defmodule MyApp.Account do
  def open(name) do
    s = "string decoy() ignored"
    build(name)
  end

  defp validate(amount) do
    if amount > 0 do
      check(amount)
    end
  end

  defmacro trace(expr) do
    instrument(expr)
  end

  def double(x), do: multiply(x, 2)

  def report() do
    MyApp.Logger.write("ok")
  end
end

defmodule Helpers do
  def assist do
    compute()
  end
end
`

func TestElixirSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.ex", elixirSample))
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
		{"Account", KindType, ""}, // defmodule MyApp.Account -> trailing segment Account
		{"Helpers", KindType, ""},
		{"open", KindFunc, "Account"},
		{"validate", KindFunc, "Account"}, // defp
		{"trace", KindFunc, "Account"},    // defmacro
		{"double", KindFunc, "Account"},   // inline def ..., do: ...
		{"report", KindFunc, "Account"},
		{"assist", KindFunc, "Helpers"},
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

func TestElixirSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.ex", elixirSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// validate nests an if/do; its span covers the body (the matching `end` tracking handled
	// the nested `do`).
	if s := got["validate"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("validate span = %d-%d, want multi-line", s.Span.StartLine, s.Span.EndLine)
	}
	// double is a single-line inline def (no block).
	if s := got["double"]; s.Span.StartLine != s.Span.EndLine {
		t.Errorf("double span = %d-%d, want a single line (inline do:)", s.Span.StartLine, s.Span.EndLine)
	}
	// Account spans the whole module.
	if s := got["Account"]; s.Span.EndLine < s.Span.StartLine+15 {
		t.Errorf("Account span = %d-%d, want to cover the whole module", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestElixirReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.ex", elixirSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"build", "check", "instrument", "multiply", "write", "compute"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	for _, bad := range []string{"decoy", "if", "def", "defmodule"} {
		if names[bad] {
			t.Errorf("decoy/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestElixirCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.ex", elixirSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["open"], "build") {
		t.Errorf("open should call build; got %+v", calls["open"])
	}
	// report calls MyApp.Logger.write -> "write".
	if !contains(calls["report"], "write") {
		t.Errorf("report should call write; got %+v", calls["report"])
	}
	if !contains(calls["assist"], "compute") {
		t.Errorf("assist should call compute; got %+v", calls["assist"])
	}
}
