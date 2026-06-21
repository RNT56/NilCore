package ast

import "testing"

// A Bash fixture exercising both function forms (`name() {` and `function name {`/`function
// name() {`), nested brace blocks for spans, the defined-function call resolution (a call to
// a function defined in this file is counted; a call to an external command like `grep` or a
// builtin like `echo` is NOT), `#` comments, quoted strings, and a heredoc body. Decoys in
// comments/strings/heredocs must NOT register.
const bashSample = `#!/usr/bin/env bash
# a comment with deploy() that must not register

setup() {
    echo "preparing"
    grep -q foo bar
}

function build {
    setup
    local msg="run deploy() in a string, ignored"
    compile
}

function compile() {
    if true; then
        emit
    fi
}

emit() {
    cat <<EOF
this heredoc mentions build but is inert
EOF
    echo done
}

main() {
    build
    emit
}
`

func TestBashSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.sh", bashSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	for _, name := range []string{"setup", "build", "compile", "emit", "main"} {
		s, ok := got[name]
		if !ok {
			t.Errorf("%s: not extracted", name)
			continue
		}
		if s.Kind != KindFunc {
			t.Errorf("%s: kind = %q, want func", name, s.Kind)
		}
		if s.Recv != "" {
			t.Errorf("%s: recv = %q, want empty (shell has no receivers)", name, s.Recv)
		}
	}
	// build's body spans multiple lines.
	if s := got["build"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("build span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestBashCallResolution(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.sh", bashSample))
	if err != nil {
		t.Fatal(err)
	}
	// build calls setup and compile — both defined functions.
	if !contains(calls["build"], "setup") {
		t.Errorf("build should call setup; got %+v", calls["build"])
	}
	if !contains(calls["build"], "compile") {
		t.Errorf("build should call compile; got %+v", calls["build"])
	}
	// compile calls emit (a defined function).
	if !contains(calls["compile"], "emit") {
		t.Errorf("compile should call emit; got %+v", calls["compile"])
	}
	// main calls build and emit.
	if !contains(calls["main"], "build") || !contains(calls["main"], "emit") {
		t.Errorf("main should call build and emit; got %+v", calls["main"])
	}
	// External commands and builtins must NOT be counted as calls.
	for _, bad := range []string{"grep", "echo"} {
		if contains(calls["setup"], bad) {
			t.Errorf("setup: external/builtin %q leaked into calls: %+v", bad, calls["setup"])
		}
	}
	// The heredoc mention of `build` inside emit must not register as a call.
	if contains(calls["emit"], "build") {
		t.Errorf("heredoc text leaked: emit should not call build; got %+v", calls["emit"])
	}
}

func TestBashReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.sh", bashSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// Only defined-function invocations survive.
	for _, want := range []string{"setup", "compile", "emit", "build"} {
		if !names[want] {
			t.Errorf("expected a reference to defined function %q; got %+v", want, refs)
		}
	}
	for _, bad := range []string{"grep", "echo", "cat", "deploy", "local"} {
		if names[bad] {
			t.Errorf("non-function token %q leaked into references: %+v", bad, refs)
		}
	}
}
