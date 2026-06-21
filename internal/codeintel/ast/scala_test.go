package ast

import "testing"

// A Scala fixture exercising the shapes the backend models: a top-level object, a `case
// class` and a `case object`, a `trait`, methods (`def` inside a type) with the enclosing
// type as receiver, a top-level `def` function, an `=`-expression-bodied def, `val`/`var`
// members, annotations, and method calls in `name(...)` and `obj.method(...)` forms. Decoys
// in `//` comments, `/* */` block comments, and strings must NOT register.
const scalaSample = `package demo

// a comment with decoy() that must not register

object Geometry {
  val pi: Double = 3.14159
  var counter = 0

  @inline
  def area(r: Double): Double = {
    val s = "string decoy() ignored"
    multiply(pi, r)
  }

  def perimeter(r: Double): Double = scale(r)
}

case class Box(width: Int, height: Int) {
  def volume: Int = {
    compute(width)
  }
}

case object Empty {
  def render(): String = {
    draw()
  }
}

trait Shape {
  def describe(): Unit = {
    log("shape")
  }
}

def topLevel(n: Int): Int = {
  helper(n)
}
`

func TestScalaSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.scala", scalaSample))
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
		{"Geometry", KindType, ""},
		{"Box", KindType, ""},
		{"Empty", KindType, ""},
		{"Shape", KindType, ""},
		{"area", KindMethod, "Geometry"},
		{"perimeter", KindMethod, "Geometry"}, // =-expression body, still a member
		{"volume", KindMethod, "Box"},
		{"render", KindMethod, "Empty"},
		{"describe", KindMethod, "Shape"},
		{"topLevel", KindFunc, ""},
		{"pi", KindConst, "Geometry"},    // val -> const
		{"counter", KindVar, "Geometry"}, // var -> var
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

func TestScalaReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.scala", scalaSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"multiply", "scale", "compute", "draw", "log", "helper"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	if names["decoy"] {
		t.Errorf("decoy leaked into references: %+v", refs)
	}
}

func TestScalaCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.scala", scalaSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["area"], "multiply") {
		t.Errorf("area should call multiply; got %+v", calls["area"])
	}
	if !contains(calls["volume"], "compute") {
		t.Errorf("volume should call compute; got %+v", calls["volume"])
	}
	if !contains(calls["topLevel"], "helper") {
		t.Errorf("topLevel should call helper; got %+v", calls["topLevel"])
	}
}
