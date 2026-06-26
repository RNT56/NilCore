package main

// autonomy.go folds the autonomy daemon's standing-objectives backlog into serve
// (Phase 16, Pillar 7 / AUTO-T06): when NILCORE_AUTONOMY is set, an IDLE serve self-
// services the operator objective backlog (managed by `nilcore objective`) through the
// SAME verified run-orchestrator, executing each objective REVERSIBLY (a disposable
// worktree, discarded) and gating only at the irreversible edge.
//
// Safety stance (the whole point):
//   - The daemon holds NO authority: it forwards an operator-authored goal to the
//     verified orchestrator, which owns verification (I2) and gating (I3). The gate is
//     HEADLESS — irreversible actions DENY-default (no human is attached to a daemon),
//     auto-proceeding ONLY for an earned boundary inside the operator envelope +
//     blast fence (wrapAutoApprove over a deny-default — it composes with Pillar 5,
//     never granting new authority).
//   - Objective CRUD is OPERATOR-ONLY (XC-T06): the daemon only RUNS the goal an
//     objective names; the backlog is written solely by the host `nilcore objective`
//     verb, never a model tool. Objective.Goal is inert data (I7) — executed, never
//     interpreted as policy.
//   - DEFAULT-OFF: with NILCORE_AUTONOMY unset the daemon is never started; even when
//     set, an empty backlog makes the source poll and emit nothing — byte-identical.

import (
	"context"
	"fmt"
	"os"
	"time"

	"nilcore/internal/agent"
	"nilcore/internal/autosrc"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/objective"
	"nilcore/internal/trigger"
)

// runAutonomyDaemon drives the autonomy daemon over the standing-objectives backlog
// until ctx is cancelled (serve shutdown), draining gracefully. The orchestrator is
// built by the caller at serve STARTUP (so a missing model key fails loudly at boot,
// not inside this goroutine); here we only open the operator store, build the backlog
// source, and run the bounded daemon. A store-open failure disables the daemon with a
// visible message rather than taking down serve.
func runAutonomyDaemon(ctx context.Context, orch *agent.Orchestrator, log *eventlog.Log) {
	s, err := openObjectiveStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: autonomy daemon disabled (store: %v)\n", err)
		return
	}
	defer s.Close()

	backlog := objective.New(s.ObjectiveStore())
	src := autosrc.NewBacklogSource(backlog, autosrc.BacklogConfig{})

	handler := func(ctx context.Context, sig trigger.Signal) error {
		// Run the operator-authored objective goal through the verified orchestrator:
		// reversible by construction (a disposable worktree), with every irreversible
		// step hitting the headless gate the caller wired onto orch.Approver.
		_, err := orch.Execute(ctx, backend.Task{
			ID:   fmt.Sprintf("auto-%d", time.Now().UnixNano()),
			Goal: sig.Goal,
		})
		return err
	}

	d := autosrc.New(handler, autosrc.Config{Concurrency: 1, Log: log})
	if err := d.Run(ctx, src); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "nilcore: autonomy daemon stopped: %v\n", err)
	}
}
