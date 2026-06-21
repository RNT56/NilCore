package ast

import "testing"

// A Zig fixture exercising: the `const NAME = struct { ... }` container idiom with methods
// (which read NAME as their receiver), a `pub fn`, a `const NAME = enum { ... }` container, a
// top-level free `fn`, `comptime`/`export` qualifiers, and call sites in `name(...)` and
// `std.debug.print(...)` forms. Decoys in comments and strings must NOT register.
const zigSample = `// a comment with decoy() that must not register
const std = @import("std");

const Point = struct {
    x: f32,
    y: f32,

    pub fn dist(self: Point) f32 {
        return compute(self.x);
    }

    fn translate(self: *Point, dx: f32) void {
        adjust(dx);
    }
};

const Color = enum {
    red,
    green,

    pub fn name(self: Color) []const u8 {
        return label(self);
    }
};

pub fn main() void {
    const s = "string decoy() ignored";
    helper();
    std.debug.print("hi", .{});
}
`

func TestZigSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.zig", zigSample))
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
		{"Point", KindType, ""},
		{"Color", KindType, ""},
		{"dist", KindMethod, "Point"},
		{"translate", KindMethod, "Point"},
		{"name", KindMethod, "Color"},
		{"main", KindFunc, ""},
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

func TestZigReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.zig", zigSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"compute", "adjust", "label", "helper", "print"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	if names["decoy"] {
		t.Errorf("decoy leaked into references: %+v", refs)
	}
}

func TestZigCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.zig", zigSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["dist"], "compute") {
		t.Errorf("dist should call compute; got %+v", calls["dist"])
	}
	if !contains(calls["main"], "helper") {
		t.Errorf("main should call helper; got %+v", calls["main"])
	}
}
