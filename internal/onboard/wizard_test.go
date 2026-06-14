package onboard

import (
	"strings"
	"testing"
)

// TestEchoOffNoopOnNonTTY proves the secret echo-off (audit L8) degrades to a
// safe no-op when input is not a terminal (a pipe or test reader), reporting
// masked=false so the caller knows masking did not take effect, so
// non-interactive provisioning is never broken by it.
func TestEchoOffNoopOnNonTTY(t *testing.T) {
	restore, masked := echoOff(strings.NewReader("piped"))
	if restore == nil {
		t.Fatal("echoOff must always return a callable restore func")
	}
	if masked {
		t.Fatal("echoOff must report masked=false off a terminal")
	}
	restore() // must not panic on a non-terminal reader
}

func TestDefaultModels(t *testing.T) {
	cases := []struct {
		providers         []string
		execWant, advWant string
	}{
		{nil, "anthropic:claude-sonnet-4-6", "anthropic:claude-opus-4-8"},
		{[]string{"openai"}, "openai:gpt-5.5", "openai:gpt-5.5"},
		{[]string{"openrouter"}, "openrouter:openrouter/fusion", "openrouter:openrouter/fusion"},
		{[]string{"openai", "anthropic"}, "anthropic:claude-sonnet-4-6", "anthropic:claude-opus-4-8"}, // anthropic wins
	}
	for _, c := range cases {
		var ps []ProviderConfig
		for _, n := range c.providers {
			ps = append(ps, ProviderConfig{Name: n, KeyRef: n + "_api_key"})
		}
		exec, adv := defaultModels(ps)
		if exec != c.execWant || adv != c.advWant {
			t.Errorf("defaultModels(%v) = %q,%q; want %q,%q", c.providers, exec, adv, c.execWant, c.advWant)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a , , b, a ,c ")
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("splitList = %v, want %v", got, want)
	}
	if splitList("") != nil {
		t.Error("splitList(\"\") must be nil")
	}
}

func TestStyle(t *testing.T) {
	on, off := style{on: true}, style{on: false}
	if g := on.colorGlyphs("  ✓ ok\n  ✗ no\n"); !strings.Contains(g, "\033[32m✓") || !strings.Contains(g, "\033[31m✗") {
		t.Errorf("on: glyphs not colored: %q", g)
	}
	if g := off.colorGlyphs("✓ ✗"); g != "✓ ✗" {
		t.Errorf("off: must be plain, got %q", g)
	}
	if on.bold("x") == "x" || on.dim("x") == "x" {
		t.Error("on: bold/dim must wrap")
	}
	if off.bold("x") != "x" || off.dim("x") != "x" {
		t.Error("off: bold/dim must be plain")
	}
}

func TestMaskTail(t *testing.T) {
	if got := maskTail("sk-ant-123456"); got != "…3456" {
		t.Errorf("maskTail = %q, want …3456", got)
	}
	if got := maskTail("abc"); got != "…" {
		t.Errorf("maskTail(short) = %q, want …", got)
	}
}
