package ast

import (
	"testing"
)

// A representative Kotlin fixture exercising the shapes the backend models: a top-level
// function, a class with members and a companion object, a `data class`, an interface, an
// `enum class`, an `object`, an extension function `fun Foo.bar()` (receiver from the
// header), an annotated method, and an `=`-expression body. Decoys in comments and strings
// must NOT register as calls; control keywords (`if`, `when`, `for`) must not read as calls.
const kotlinSample = `package com.example

import kotlin.math.max

// a comment with decoy() that must not register
fun topLevel(n: Int): Int {
    return helper(n)
}

class Box<T>(private val value: T) {
    @Deprecated("x")
    fun map(f: (T) -> T): T {
        /* block decoy() ignored */
        val s = "string decoy() ignored"
        return transform(f(value))
    }

    fun shout() = format(value)

    private fun transform(x: T): T = x
    private fun format(x: T): T = x

    companion object {
        fun create(): Box<Int> = Box(0)
    }
}

fun Box<Int>.doubled(): Int {
    return compute(0)
}

data class Point(val x: Int, val y: Int)

interface Shape {
    fun area(): Double
}

enum class Color {
    RED, GREEN, BLUE
}

object Registry {
    fun register() {
        store()
    }
}
`

func TestKotlinSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.kt", kotlinSample))
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
		{"shout", KindMethod, "Box"}, // `=`-expression body, still a member
		{"transform", KindMethod, "Box"},
		{"create", KindMethod, "Box"},  // inside companion object -> enclosing type Recv
		{"doubled", KindMethod, "Box"}, // extension fun Box.doubled -> Recv from header
		{"Point", KindType, ""},
		{"Shape", KindType, ""},
		{"Color", KindType, ""},
		{"Registry", KindType, ""},
		{"register", KindMethod, "Registry"},
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

func TestKotlinSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.kt", kotlinSample))
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
		t.Errorf("Box type span = %d-%d, want it to cover the whole class body", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestKotlinReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.kt", kotlinSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// transform(...) in map; compute(...) in the extension; store() in register.
	for _, want := range []string{"transform", "compute", "store", "helper"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	for _, bad := range []string{"decoy", "map", "if", "when", "for"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestKotlinCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.kt", kotlinSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["map"], "transform") {
		t.Errorf("map should call transform; got %+v", calls["map"])
	}
	if !contains(calls["doubled"], "compute") {
		t.Errorf("doubled should call compute; got %+v", calls["doubled"])
	}
	if !contains(calls["register"], "store") {
		t.Errorf("register should call store; got %+v", calls["register"])
	}
	for _, fn := range []string{"map", "doubled", "register"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}
