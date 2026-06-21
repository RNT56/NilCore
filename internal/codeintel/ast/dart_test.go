package ast

import "testing"

// A Dart fixture exercising: a class with a constructor, a method, a getter and a setter; a
// mixin; an enum; a top-level function; an extension on a type; `@override` annotation;
// `async` bodies; and `=>` arrow bodies. Calls appear in `name(...)` and `obj.method(...)`
// forms. Decoys in comments and strings must NOT register.
const dartSample = `// a comment with decoy() that must not register

class Account {
  int balance;

  Account(this.balance);

  @override
  void deposit(int amount) {
    record(amount);
  }

  int get current => balance;

  set current(int v) {
    update(v);
  }

  Future<void> sync() async {
    await refresh();
  }
}

mixin Loggable {
  void log(String msg) {
    emit(msg);
  }
}

enum Color { red, green, blue }

extension StringExt on String {
  String shout() {
    return upper();
  }
}

void topLevel() {
  var s = "string decoy() ignored";
  helper();
}
`

// has reports whether syms contains a symbol with the given name/kind/recv.
func has(syms []Symbol, name string, kind Kind, recv string) bool {
	for _, s := range syms {
		if s.Name == name && s.Kind == kind && s.Recv == recv {
			return true
		}
	}
	return false
}

func TestDartSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.dart", dartSample))
	if err != nil {
		t.Fatal(err)
	}

	// Types. (Account appears both as a type and as a constructor member, so we assert with
	// the full name/kind/recv triple rather than a name-keyed map that would collide.)
	tests := []struct {
		name string
		kind Kind
		recv string
	}{
		{"Account", KindType, ""},
		{"Loggable", KindType, ""},
		{"Color", KindType, ""},
		{"deposit", KindMethod, "Account"},
		{"current", KindMethod, "Account"}, // getter/setter -> method on Account
		{"sync", KindMethod, "Account"},
		{"log", KindMethod, "Loggable"},
		{"shout", KindMethod, "String"}, // extension on String -> Recv String
		{"topLevel", KindFunc, ""},
		{"Account", KindMethod, "Account"}, // the constructor, captured on the class
	}
	for _, tc := range tests {
		if !has(syms, tc.name, tc.kind, tc.recv) {
			t.Errorf("missing symbol %s (kind %q, recv %q); got %+v", tc.name, tc.kind, tc.recv, syms)
		}
	}
}

func TestDartReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.dart", dartSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"record", "update", "refresh", "emit", "upper", "helper"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	if names["decoy"] {
		t.Errorf("decoy leaked into references: %+v", refs)
	}
}

func TestDartCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.dart", dartSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["deposit"], "record") {
		t.Errorf("deposit should call record; got %+v", calls["deposit"])
	}
	if !contains(calls["log"], "emit") {
		t.Errorf("log should call emit; got %+v", calls["log"])
	}
	if !contains(calls["topLevel"], "helper") {
		t.Errorf("topLevel should call helper; got %+v", calls["topLevel"])
	}
}
