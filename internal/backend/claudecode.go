package backend

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// ClaudeCode delegates the task to Claude Code in headless mode:
//
//	claude -p "<goal>" --output-format stream-json --permission-mode acceptEdits
//
// As with the Codex adapter, production runs this inside the sandbox container
// with ANTHROPIC_API_KEY injected per run. Phase 0 ships the exec + parse seam;
// it reuses lastEventText/digText/tailStr from codex.go (same package).
type ClaudeCode struct {
	Bin string // defaults to "claude"
}

func (c *ClaudeCode) Name() string { return "claude-code" }

func (c *ClaudeCode) Run(ctx context.Context, t Task) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, "-p", t.Goal,
		"--output-format", "stream-json",
		"--permission-mode", "acceptEdits")
	cmd.Dir = t.Dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{Backend: c.Name()}, fmt.Errorf("claude -p: %w (%s)", err, tailStr(stderr.String(), 500))
	}
	return Result{Backend: c.Name(), Summary: lastEventText(stdout.String()), SelfClaimed: true}, nil
}
