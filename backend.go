// Package backend defines the coding-task contract and its implementations.
// Every backend — the native loop, Codex, and Claude Code — satisfies the same
// CodingBackend interface. That single seam is what makes the hybrid design
// clean: the orchestrator picks a backend; nothing else changes.
package backend

import "context"

// Task is a unit of coding work, executed inside Dir (normally a git worktree).
type Task struct {
	ID          string
	Goal        string   // natural-language instruction
	Dir         string   // absolute path to the worktree on the host
	Constraints []string // optional guardrails surfaced to the model
}

// Result is what a backend returns. The orchestrator's verifier — not this
// self-report — decides whether the work actually ships.
type Result struct {
	Backend     string
	Summary     string // the backend's own account of what it did
	SelfClaimed bool   // the backend believes the task is complete
}

// CodingBackend turns a Task into a Result by modifying files under Task.Dir.
type CodingBackend interface {
	Name() string
	Run(ctx context.Context, t Task) (Result, error)
}
