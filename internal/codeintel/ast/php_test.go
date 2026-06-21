package ast

import (
	"testing"
)

// A representative PHP fixture exercising the shapes the backend models: a namespace, a
// free function, a class with a constructor and methods (one calling a helper via
// `$this->`, one static method called via `Class::`), an interface, a trait, and an enum.
// Decoys in `//`, `#`, `/* */`, and string contexts must NOT register as calls; the PHP
// tags must be stripped; control keywords (`if`, `foreach`, `new`) must not read as calls.
const phpSample = `<?php

namespace App\Models;

// a comment with decoy() that must not register
# a hash comment with decoy2() that must not register

function topLevel(int $n): int {
    return helper($n);
}

class User {
    private string $name;

    public function __construct(string $name) {
        $this->name = $name;
    }

    public function greet(): string {
        /* block decoy() ignored */
        $s = "string decoy() ignored";
        return $this->format($this->name);
    }

    public static function make(string $name): User {
        return new User($name);
    }

    private function format(string $x): string {
        if ($x === "") {
            return "anon";
        }
        return $x;
    }
}

interface Greeter {
    public function salute(): string;
}

trait Loggable {
    public function log(string $msg): void {
        Logger::write($msg);
    }
}

enum Suit {
    case Hearts;
    case Spades;
}
?>
`

func TestPHPSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.php", phpSample))
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
		{"Models", KindType, ""}, // namespace -> trailing segment
		{"topLevel", KindFunc, ""},
		{"User", KindType, ""},
		{"greet", KindMethod, "User"},
		{"make", KindMethod, "User"},
		{"format", KindMethod, "User"},
		{"Greeter", KindType, ""},
		{"Loggable", KindType, ""},
		{"log", KindMethod, "Loggable"},
		{"Suit", KindType, ""},
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

	// The constructor `__construct` is a method on User.
	var sawCtor bool
	for _, s := range syms {
		if s.Name == "__construct" && s.Kind == KindMethod && s.Recv == "User" {
			sawCtor = true
		}
	}
	if !sawCtor {
		t.Errorf("constructor __construct not captured as a method with recv User: %+v", syms)
	}
}

func TestPHPSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.php", phpSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	if s := got["greet"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("greet span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
	if s := got["User"]; s.Span.EndLine < s.Span.StartLine+10 {
		t.Errorf("User type span = %d-%d, want it to cover the whole class body", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestPHPReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.php", phpSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// helper($n) bare call; $this->format(...) -> "format"; Logger::write(...) -> "write".
	for _, want := range []string{"helper", "format", "write"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys, self-call names, and control keywords must not register.
	for _, bad := range []string{"decoy", "decoy2", "greet", "if", "foreach", "new"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestPHPCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.php", phpSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["greet"], "format") {
		t.Errorf("greet should call format ($this->format); got %+v", calls["greet"])
	}
	if !contains(calls["log"], "write") {
		t.Errorf("log should call write (Logger::write); got %+v", calls["log"])
	}
	if !contains(calls["topLevel"], "helper") {
		t.Errorf("topLevel should call helper; got %+v", calls["topLevel"])
	}
	for _, fn := range []string{"greet", "log", "topLevel"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}
