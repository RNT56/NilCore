package ast

import "testing"

// A Lua fixture exercising: a top-level `function`, a `local function`, `function Tbl.method`
// (dot form) and `function Tbl:method` (colon form, both with Recv Tbl), a `Name = function`
// assignment, nesting via `if`/`for`/`do`/`while` openers and matching `end`, and calls in
// `name(...)`, `obj.method(...)`, `obj:method(...)` forms. Decoys in `--` comments,
// `--[[ ]]` block comments, and strings must NOT register.
const luaSample = `-- a comment with decoy() that must not register
--[[
block comment with decoy2() ignored
]]

function top_level(n)
  return helper(n)
end

local function priv(x)
  return inner(x)
end

Account = {}

function Account.create(name)
  local s = "string decoy() ignored"
  return build(name)
end

function Account:deposit(amount)
  if amount > 0 then
    for i = 1, amount do
      self:record(i)
    end
  end
end

handler = function(evt)
  return process(evt)
end
`

func TestLuaSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.lua", luaSample))
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
		{"priv", KindFunc, ""},
		{"create", KindMethod, "Account"},  // function Account.create
		{"deposit", KindMethod, "Account"}, // function Account:deposit
		{"handler", KindFunc, ""},          // handler = function(...)
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

func TestLuaSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.lua", luaSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// deposit nests if/for/do; its span must cover the whole body, with the matching `end`
	// tracking having correctly balanced the nested openers.
	if s := got["deposit"]; s.Span.EndLine <= s.Span.StartLine+2 {
		t.Errorf("deposit span = %d-%d, want a multi-line body covering the nested blocks", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestLuaReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.lua", luaSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"helper", "inner", "build", "record", "process"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	for _, bad := range []string{"decoy", "decoy2", "if", "for", "end"} {
		if names[bad] {
			t.Errorf("decoy/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestLuaCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.lua", luaSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["top_level"], "helper") {
		t.Errorf("top_level should call helper; got %+v", calls["top_level"])
	}
	if !contains(calls["create"], "build") {
		t.Errorf("create should call build; got %+v", calls["create"])
	}
	// deposit calls self:record -> "record".
	if !contains(calls["deposit"], "record") {
		t.Errorf("deposit should call record; got %+v", calls["deposit"])
	}
}
