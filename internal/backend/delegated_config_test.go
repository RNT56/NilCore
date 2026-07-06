package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// TestCodexArgs covers the pure command builder: the all-zero case is
// byte-identical to the historical default, each knob appears only when set, and
// hostile values stay a single shell-quoted argument (no `sh -c` breakout).
func TestCodexArgs(t *testing.T) {
	tests := []struct {
		name   string
		goal   string
		model  string
		effort string
		extra  []string
		want   string
	}{
		{
			name: "all zero is byte-identical to the original",
			goal: "fix the bug",
			want: "codex exec --json --full-auto 'fix the bug'",
		},
		{
			name:  "model only",
			goal:  "g",
			model: "o4-mini",
			want:  "codex exec --json --full-auto --model 'o4-mini' 'g'",
		},
		{
			name:   "effort only",
			goal:   "g",
			effort: "high",
			want:   "codex exec --json --full-auto -c model_reasoning_effort='high' 'g'",
		},
		{
			name:  "extra args appended verbatim and quoted",
			goal:  "g",
			extra: []string{"-c", "sandbox_mode=danger", "--foo"},
			want:  "codex exec --json --full-auto '-c' 'sandbox_mode=danger' '--foo' 'g'",
		},
		{
			name:   "all knobs together, in order",
			goal:   "g",
			model:  "gpt-5",
			effort: "low",
			extra:  []string{"--verbose"},
			want:   "codex exec --json --full-auto --model 'gpt-5' -c model_reasoning_effort='low' '--verbose' 'g'",
		},
		{
			name:  "hostile model value cannot break out of sh -c",
			goal:  "g",
			model: "x'; rm -rf / #",
			want:  `codex exec --json --full-auto --model 'x'\''; rm -rf / #' 'g'`,
		},
		{
			name:  "extra arg with a space stays one argument",
			goal:  "g",
			extra: []string{"a b ; c"},
			want:  "codex exec --json --full-auto 'a b ; c' 'g'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexArgs(tt.goal, tt.model, tt.effort, tt.extra); got != tt.want {
				t.Errorf("codexArgs() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// TestClaudeArgs mirrors TestCodexArgs for the Claude Code builder. Effort is
// deliberately absent here — it travels via env, not a flag.
func TestClaudeArgs(t *testing.T) {
	// --verbose is mandatory alongside stream-json under -p, so it is part of the
	// baseline command every knob-combination builds on.
	const base = "claude -p 'g' --output-format stream-json --verbose --permission-mode acceptEdits"
	tests := []struct {
		name  string
		goal  string
		model string
		extra []string
		want  string
	}{
		{
			name: "all zero is the default command (stream-json + verbose)",
			goal: "g",
			want: base,
		},
		{
			name:  "model appended",
			goal:  "g",
			model: "claude-sonnet-4-5",
			want:  base + " --model 'claude-sonnet-4-5'",
		},
		{
			name:  "extra args appended verbatim and quoted",
			goal:  "g",
			extra: []string{"--max-turns", "10"},
			want:  base + " '--max-turns' '10'",
		},
		{
			name:  "hostile model value cannot break out of sh -c",
			goal:  "g",
			model: "x'; rm -rf / #",
			want:  base + ` --model 'x'\''; rm -rf / #'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeArgs(tt.goal, tt.model, tt.extra); got != tt.want {
				t.Errorf("claudeArgs() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// TestMergeEnv asserts the per-run secret always wins (merged LAST) and that the
// base map is copied, not mutated.
func TestMergeEnv(t *testing.T) {
	t.Run("nil base yields a one-entry map", func(t *testing.T) {
		got := mergeEnv(nil, "K", "v")
		if len(got) != 1 || got["K"] != "v" {
			t.Errorf("mergeEnv(nil) = %v", got)
		}
	})
	t.Run("secret wins over an operator override and base is not mutated", func(t *testing.T) {
		base := map[string]string{"CODEX_API_KEY": "attacker", "X": "y"}
		got := mergeEnv(base, "CODEX_API_KEY", "real-secret")
		if got["CODEX_API_KEY"] != "real-secret" {
			t.Errorf("key did not win: %v", got)
		}
		if got["X"] != "y" {
			t.Errorf("base entry lost: %v", got)
		}
		if base["CODEX_API_KEY"] != "attacker" {
			t.Errorf("base map was mutated: %v", base)
		}
	})
}

// TestCodexRunByteIdenticalWhenAllFieldsZero proves that the full Run path with
// every new field zero produces the exact historical command and an env map of
// just {CODEX_API_KEY}.
func TestCodexRunByteIdenticalWhenAllFieldsZero(t *testing.T) {
	box := &fakeBox{stdout: `{"text":"ok"}`}
	cx := &Codex{Box: box, Key: "sk-codex"}
	if _, err := cx.Run(context.Background(), Task{ID: "t", Goal: "fix the bug"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "codex exec --json --full-auto 'fix the bug'"; box.gotCmd != want {
		t.Errorf("cmd =\n  %q\nwant\n  %q", box.gotCmd, want)
	}
	if len(box.gotEnv) != 1 || box.gotEnv["CODEX_API_KEY"] != "sk-codex" {
		t.Errorf("env = %v, want exactly {CODEX_API_KEY: sk-codex}", box.gotEnv)
	}
}

// TestClaudeRunDefaultCommandWhenAllFieldsZero is the Claude Code counterpart: with
// every operator knob zero, Run emits the default command — stream-json plus the
// mandatory --verbose (the CLI rejects stream-json under -p without it).
func TestClaudeRunDefaultCommandWhenAllFieldsZero(t *testing.T) {
	box := &fakeBox{stdout: `{"text":"ok"}`}
	cc := &ClaudeCode{Box: box, Key: "sk-ant"}
	if _, err := cc.Run(context.Background(), Task{ID: "t", Goal: "do it"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "claude -p 'do it' --output-format stream-json --verbose --permission-mode acceptEdits"; box.gotCmd != want {
		t.Errorf("cmd =\n  %q\nwant\n  %q", box.gotCmd, want)
	}
	if len(box.gotEnv) != 1 || box.gotEnv["ANTHROPIC_API_KEY"] != "sk-ant" {
		t.Errorf("env = %v, want exactly {ANTHROPIC_API_KEY: sk-ant}", box.gotEnv)
	}
}

// TestCodexRunMergesEnvAndKeyWins exercises the full Run env-merge: operator Env
// is present, and the injected key still wins even when Env tries to shadow it.
func TestCodexRunMergesEnvAndKeyWins(t *testing.T) {
	box := &fakeBox{stdout: `{"text":"ok"}`}
	cx := &Codex{
		Box:    box,
		Key:    "real-secret",
		Model:  "o4-mini",
		Effort: "high",
		Env:    map[string]string{"CODEX_HOME": "/cfg", "CODEX_API_KEY": "attacker"},
	}
	if _, err := cx.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if box.gotEnv["CODEX_HOME"] != "/cfg" {
		t.Errorf("operator env not merged: %v", box.gotEnv)
	}
	if box.gotEnv["CODEX_API_KEY"] != "real-secret" {
		t.Errorf("injected key must win over Env shadow, got %q", box.gotEnv["CODEX_API_KEY"])
	}
	if !strings.Contains(box.gotCmd, "--model 'o4-mini'") || !strings.Contains(box.gotCmd, "model_reasoning_effort='high'") {
		t.Errorf("knobs missing from cmd: %q", box.gotCmd)
	}
}

// TestClaudeRunEffortGoesToEnvAndKeyWins verifies Effort lands in
// CLAUDE_CODE_EFFORT_LEVEL (not the command) and the key wins last.
func TestClaudeRunEffortGoesToEnvAndKeyWins(t *testing.T) {
	box := &fakeBox{stdout: `{"text":"ok"}`}
	cc := &ClaudeCode{
		Box:    box,
		Key:    "real-secret",
		Model:  "claude-opus-4",
		Effort: "high",
		Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/cfg", "ANTHROPIC_API_KEY": "attacker"},
	}
	if _, err := cc.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if box.gotEnv["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Errorf("effort not in env: %v", box.gotEnv)
	}
	if box.gotEnv["CLAUDE_CONFIG_DIR"] != "/cfg" {
		t.Errorf("operator env not merged: %v", box.gotEnv)
	}
	if box.gotEnv["ANTHROPIC_API_KEY"] != "real-secret" {
		t.Errorf("injected key must win over Env shadow, got %q", box.gotEnv["ANTHROPIC_API_KEY"])
	}
	// Effort must NOT leak into the command (flag name varies across versions).
	if strings.Contains(box.gotCmd, "high") || strings.Contains(box.gotCmd, "effort") {
		t.Errorf("effort leaked into the command: %q", box.gotCmd)
	}
	if !strings.Contains(box.gotCmd, "--model 'claude-opus-4'") {
		t.Errorf("model missing from cmd: %q", box.gotCmd)
	}
}

// TestDelegatedConfigNeverLogsSecretsOrKnobs guards invariant I3: with model,
// effort, extra args, and an env secret all set, the event log still carries only
// {cli, exit} — never the key, model, effort, or any env value.
func TestDelegatedConfigNeverLogsSecretsOrKnobs(t *testing.T) {
	const (
		key       = "sk-super-secret"
		envSecret = "env-secret-value"
	)
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	box := &fakeBox{stdout: `{"text":"ok"}`}
	cx := &Codex{
		Box:       box,
		Key:       key,
		Log:       log,
		Model:     "secret-model-id",
		Effort:    "secret-effort",
		ExtraArgs: []string{"--secret-flag"},
		Env:       map[string]string{"CODEX_HOME": envSecret},
	}
	if _, err := cx.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	b, _ := os.ReadFile(logPath)
	logged := string(b)
	for _, leak := range []string{key, envSecret, "secret-model-id", "secret-effort", "--secret-flag"} {
		if strings.Contains(logged, leak) {
			t.Fatalf("event log leaked %q: %s", leak, logged)
		}
	}
}
