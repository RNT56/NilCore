// Package verify is the source of truth for "done". Whatever produced a change
// — the native loop, Codex, or Claude Code — its work only ships if these checks
// pass. That is what keeps delegation to black-box coding agents robust: their
// self-report never decides the outcome.
package verify

import (
	"context"
	"strings"

	"nullclaw/internal/sandbox"
)

// Report is the result of running the project's checks.
type Report struct {
	Passed bool
	Output string // tail of combined output, fed back to the agent on failure
}

// Verifier runs a project's checks and reports pass/fail.
type Verifier interface {
	Check(ctx context.Context) (Report, error)
}

// CommandVerifier runs a single check command inside the sandbox. Example:
// "make verify" or "go build ./... && go vet ./... && go test ./...".
type CommandVerifier struct {
	Box     sandbox.Sandbox
	Command string
}

func New(box sandbox.Sandbox, command string) *CommandVerifier {
	if command == "" {
		command = "make verify"
	}
	return &CommandVerifier{Box: box, Command: command}
}

func (v *CommandVerifier) Check(ctx context.Context) (Report, error) {
	res, err := v.Box.Exec(ctx, v.Command)
	if err != nil {
		return Report{}, err
	}
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	return Report{Passed: res.ExitCode == 0, Output: tail(out, 4000)}, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}
