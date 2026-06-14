// Package agent wires a task through a backend and the verifier, recording every
// step. Phase 0 runs the configured backend once. The adaptive routing policy —
// a single backend by default, race-and-pick on hard or failed tasks, and
// cross-model review at the irreversible gate — slots in here later without
// changing the Task/Result contract.
package agent

import (
	"context"
	"fmt"

	"nullclaw/internal/backend"
	"nullclaw/internal/eventlog"
	"nullclaw/internal/verify"
)

type Orchestrator struct {
	Backend  backend.CodingBackend
	Verifier verify.Verifier
	Log      *eventlog.Log
}

// Outcome is the final, verifier-confirmed result of a task.
type Outcome struct {
	Backend  string
	Summary  string
	Verified bool   // did the project's checks pass after the backend ran?
	Detail   string // verifier output (tail) when it did not pass
}

func (o *Orchestrator) Execute(ctx context.Context, t backend.Task) (Outcome, error) {
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "task_start",
		Detail: map[string]any{"goal": t.Goal, "backend": o.Backend.Name()}})

	res, err := o.Backend.Run(ctx, t)
	if err != nil {
		return Outcome{Backend: o.Backend.Name()}, fmt.Errorf("backend: %w", err)
	}

	// Source of truth: re-run the project's checks no matter which backend ran.
	// This is what makes delegating to Codex or Claude Code safe — their
	// self-report never decides whether the work ships.
	rep, err := o.Verifier.Check(ctx)
	if err != nil {
		return Outcome{Backend: res.Backend, Summary: res.Summary}, fmt.Errorf("final verify: %w", err)
	}
	o.Log.Append(eventlog.Event{Task: t.ID, Backend: res.Backend, Kind: "final_verify",
		Detail: map[string]any{"passed": rep.Passed}})

	return Outcome{
		Backend:  res.Backend,
		Summary:  res.Summary,
		Verified: rep.Passed,
		Detail:   rep.Output,
	}, nil
}
