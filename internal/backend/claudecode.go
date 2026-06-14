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
// prompted. It reuses lastEventText/digText/tailStr/shellQuote from codex.go.
type ClaudeCode struct {
	Box sandbox.Sandbox
	Key string // ANTHROPIC_API_KEY, injected per run; never logged
	Log *eventlog.Log
}

func (c *ClaudeCode) Name() string { return "claude-code" }

func (c *ClaudeCode) Run(ctx context.Context, t Task) (Result, error) {
	if err := ensureCLI(ctx, c.Box, "claude"); err != nil {
		return Result{Backend: c.Name()}, err
	}
	cmd := "claude -p " + shellQuote(t.Goal) + " --output-format stream-json --permission-mode acceptEdits"
	out, err := c.Box.ExecWithEnv(ctx, cmd, map[string]string{"ANTHROPIC_API_KEY": c.Key})
	if err != nil {
		return Result{Backend: c.Name()}, fmt.Errorf("claude -p: %w", err)
	}
	c.Log.Append(eventlog.Event{Task: t.ID, Backend: c.Name(), Kind: "tool_exec",
		Detail: map[string]any{"cli": "claude", "exit": out.ExitCode}})
	if out.ExitCode != 0 {
		return Result{Backend: c.Name(), Summary: tailStr(out.Stderr, 500)}, nil
	}
	return Result{Backend: c.Name(), Summary: lastEventText(out.Stdout), SelfClaimed: true}, nil
}
