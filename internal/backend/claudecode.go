package backend

import (
	"context"
	"fmt"

	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
)

// ClaudeCode delegates the task to Claude Code in headless mode:
//
//	claude -p "<goal>" --output-format stream-json --permission-mode acceptEdits
//
// Like the Codex adapter it runs *inside* the sandbox container with
// ANTHROPIC_API_KEY injected per run (P2-T03) — never persisted, logged, or
// prompted. It reuses lastEventText/digText/tailStr/shellQuote/mergeEnv from
// codex.go.
//
// The Model/Effort/ExtraArgs/Env fields are additive operator knobs (R1): with
// all four zero the emitted command and env are byte-identical to the historical
// default. Effort is passed as the CLAUDE_CODE_EFFORT_LEVEL env var rather than a
// flag, because the effort flag name varies across CLI versions — the env var is
// the stable surface. Every interpolated command value flows through shellQuote,
// so a model id or extra arg cannot break out of `sh -c`.
type ClaudeCode struct {
	Box sandbox.Sandbox
	Key string // ANTHROPIC_API_KEY, injected per run; never logged
	Log *eventlog.Log

	Model     string            // delegated model id; "" = the CLI's default
	Effort    string            // reasoning effort; "" = default (passed as env, not flag)
	ExtraArgs []string          // raw operator-supplied extra CLI tokens, appended verbatim
	Env       map[string]string // extra per-run env merged with the API key; values never logged (I3)
}

func (c *ClaudeCode) Name() string { return "claude-code" }

func (c *ClaudeCode) Run(ctx context.Context, t Task) (Result, error) {
	if err := ensureCLI(ctx, c.Box, "claude"); err != nil {
		return Result{Backend: c.Name()}, err
	}
	cmd := claudeArgs(t.Goal, c.Model, c.ExtraArgs)
	// Start from the operator Env, layer effort (when set) as CLAUDE_CODE_EFFORT_LEVEL,
	// then the per-run key LAST so the injected secret always wins (I3).
	base := c.Env
	if c.Effort != "" {
		base = mergeEnv(base, "CLAUDE_CODE_EFFORT_LEVEL", c.Effort)
	}
	env := mergeEnv(base, "ANTHROPIC_API_KEY", c.Key)
	out, err := c.Box.ExecWithEnv(ctx, cmd, env)
	if err != nil {
		return Result{Backend: c.Name()}, fmt.Errorf("claude -p: %w", err)
	}
	// Log the run WITHOUT the key, model, effort, or env — invariant I3.
	c.Log.Append(eventlog.Event{Task: t.ID, Backend: c.Name(), Kind: "tool_exec",
		Detail: map[string]any{"cli": "claude", "exit": out.ExitCode}})
	if out.ExitCode != 0 {
		return Result{Backend: c.Name(), Summary: tailStr(out.Stderr, 500)}, nil
	}
	return Result{Backend: c.Name(), Summary: lastEventText(out.Stdout), SelfClaimed: true}, nil
}

// claudeArgs builds the full `sh -c` command for the Claude Code CLI. Pure
// helper (no sandbox, no env) for unit-testability. With model/extra zero it
// returns exactly the original command. Effort is intentionally NOT here — it
// travels via env (see the struct doc). Every value is shellQuote'd.
func claudeArgs(goal, model string, extra []string) string {
	cmd := "claude -p " + shellQuote(goal) + " --output-format stream-json --permission-mode acceptEdits"
	if model != "" {
		cmd += " --model " + shellQuote(model)
	}
	for _, a := range extra {
		cmd += " " + shellQuote(a)
	}
	return cmd
}
