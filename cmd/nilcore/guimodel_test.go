package main

import "testing"

func TestGUIModelSpec(t *testing.T) {
	// flag wins over env over default.
	if got := guiModelSpec("anthropic:claude-fable-5", "x"); got != "anthropic:claude-fable-5" {
		t.Fatalf("flag should win: %q", got)
	}
	if got := guiModelSpec("", "claude-sonnet-4-6"); got != "claude-sonnet-4-6" {
		t.Fatalf("env should win when no flag: %q", got)
	}
	if got := guiModelSpec("  ", "  "); got != defaultGUIModel {
		t.Fatalf("default should apply when neither set: %q, want %q", got, defaultGUIModel)
	}
	if defaultGUIModel != "claude-opus-4-8" {
		t.Fatalf("GUI default = %q, want a strong model (Opus 4.8)", defaultGUIModel)
	}
}
