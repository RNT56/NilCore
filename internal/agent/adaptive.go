package agent

import (
	"context"
	"fmt"
	"strings"

	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/spawn"
	"nilcore/internal/summarize"
)

// executePlanned decomposes a complex goal (planner), runs its subtasks in
// parallel worktrees (spawner via RunSub), records statuses on the blackboard,
// and aggregates. handled is false (nil error) when planning isn't usable, so
// Execute falls back to the single-task path. The verifier stays the gate: each
// subtask verifies its own worktree inside RunSub.
func (o *Orchestrator) executePlanned(ctx context.Context, t backend.Task) (Outcome, bool, error) {
	if o.RunSub == nil {
		return Outcome{}, false, nil
	}
	tree, err := o.Plan(ctx, t.Goal)
	if err != nil {
		o.Log.Append(eventlog.Event{Task: t.ID, Kind: "plan_failed",
			Detail: map[string]any{"error": err.Error()}})
		return Outcome{}, false, nil // fall back to the single-task path
	}
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "planned",
		Detail: map[string]any{"subtasks": len(tree.Tasks)}})

	subs := spawn.FromPlan(tree, summarize.ContextSummary{Goal: t.Goal})
	sp := &spawn.Spawner{MaxConcurrent: o.MaxParallel, Run: o.RunSub}
	results := sp.Spawn(ctx, subs)

	allPassed := len(results) > 0
	var b strings.Builder
	for _, r := range results {
		status := "passed"
		if r.Err != nil || !r.Passed {
			allPassed = false
			status = "failed"
		}
		if o.Board != nil {
			_ = o.Board.SetStatus(ctx, r.ID, t.Goal, status)
		}
		fmt.Fprintf(&b, "- %s: %s\n", r.ID, status)
	}
	o.Log.Append(eventlog.Event{Task: t.ID, Kind: "planned_done",
		Detail: map[string]any{"passed": allPassed}})

	return Outcome{Backend: "planned", Summary: b.String(), Verified: allPassed}, true, nil
}
