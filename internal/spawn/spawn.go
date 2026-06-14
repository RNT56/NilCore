// Package spawn runs subtasks as scoped subworkers in parallel worktrees with
// budgets, isolating failures and aggregating results (P3-T02). Each subworker is
// seeded with a ContextSummary (fresh context, not the parent's transcript) and
// runs under a concurrency cap and a per-task time budget; one subworker failing
// (even panicking) never crashes the run. The orchestrator supplies RunFunc (it
// owns worktree + backend creation); spawn owns concurrency, budgets, isolation,
// and aggregation.
package spawn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"nilcore/internal/planner"
	"nilcore/internal/summarize"
)

// Subtask is one unit of work, seeded with the context it needs.
type Subtask struct {
	ID      string
	Goal    string
	Summary summarize.ContextSummary
}

// Result is a subworker's outcome. Err is set when the subworker failed or
// panicked — isolated, not propagated.
type Result struct {
	ID      string
	Summary string
	Passed  bool
	Err     error
}

// RunFunc runs one subtask in its own worktree and returns its result.
type RunFunc func(ctx context.Context, st Subtask) Result

// Spawner runs subtasks concurrently under a cap and a per-task time budget.
type Spawner struct {
	MaxConcurrent int           // parallel subworker cap (default 1)
	Timeout       time.Duration // per-subtask wall-clock budget (0 = none)
	Run           RunFunc
}

// Spawn runs all subtasks and returns their results in input order. A failing or
// panicking subworker is recorded as a Result with Err set — never fatal.
func (s *Spawner) Spawn(ctx context.Context, subtasks []Subtask) []Result {
	mc := s.MaxConcurrent
	if mc < 1 {
		mc = 1
	}
	sem := make(chan struct{}, mc)
	results := make([]Result, len(subtasks))
	var wg sync.WaitGroup

	for i := range subtasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			st := subtasks[i]
			defer func() {
				if r := recover(); r != nil {
					results[i] = Result{ID: st.ID, Err: fmt.Errorf("subworker panicked: %v", r)}
				}
			}()

			sctx := ctx
			if s.Timeout > 0 {
				var cancel context.CancelFunc
				sctx, cancel = context.WithTimeout(ctx, s.Timeout)
				defer cancel()
			}
			results[i] = s.Run(sctx, st)
		}(i)
	}
	wg.Wait()
	return results
}

// FromPlan converts a planner.Tree into subtasks, each seeded with a base summary
// whose goal is set to the subtask's goal.
func FromPlan(tree planner.Tree, base summarize.ContextSummary) []Subtask {
	out := make([]Subtask, 0, len(tree.Tasks))
	for _, t := range tree.Tasks {
		s := base
		s.Goal = t.Goal
		if t.Acceptance != "" {
			s.Remaining = "Acceptance: " + t.Acceptance
		}
		out = append(out, Subtask{ID: t.ID, Goal: t.Goal, Summary: s})
	}
	return out
}
