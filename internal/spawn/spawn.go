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

// Subtask is one unit of work, seeded with the context it needs. DependsOn names
// the IDs whose merged work this subtask is built on top of; the DAGScheduler
// holds a node back until every dependency has Passed (and been integrated).
type Subtask struct {
	ID        string
	Goal      string
	DependsOn []string
	Summary   summarize.ContextSummary
}

// State is a node's single terminal outcome under DAG scheduling. Every node
// ends in exactly one of these (termination by construction). The flat Spawner
// leaves it Pending — it has no dependency notion — and reports via Passed/Err.
type State string

const (
	StatePending State = ""        // not yet (or never) resolved by the DAG
	StatePassed  State = "passed"  // ran and Passed==true
	StateFailed  State = "failed"  // ran and Passed==false (or Err set)
	StateSkipped State = "skipped" // a dependency failed/was skipped/cyclic
	StateCycle   State = "cycle"   // part of (or downstream of) a dependency cycle
)

// Result is a subworker's outcome. Err is set when the subworker failed or
// panicked — isolated, not propagated. Branch is the task branch the subworker's
// verified commit lives on (set by the run func; consumed by the integrator).
// State is the DAG terminal outcome (Pending for the flat Spawner).
type Result struct {
	ID      string
	Summary string
	Branch  string
	Passed  bool
	State   State
	Err     error
	// Artifact is the typed, verifier-set projection of a subworker's evidence
	// artifact (Pillar 3). It is nil for every non-typed-research run — which is
	// the default — and `json:",omitempty"` plus pointer keeps the serialized
	// shape byte-identical when off (no "artifact" key at all). The supervisor's
	// renderReport treats these fields as TRUSTED control lines because they are
	// harness-computed (status set by the ArtifactVerifier), never model
	// self-claimed. Carried verbatim through Spawn and a DAG wave.
	Artifact *ArtifactSummary `json:",omitempty"`
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
		// Acquire a slot, but honor cancellation: a cancelled ctx must not block the
		// dispatcher on a full semaphore. On cancel, record a terminal (Skipped)
		// Result for this and every remaining subtask so the slice stays
		// len(subtasks) and callers see a cancelled outcome, then drain the
		// already-launched goroutines and return.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			for j := i; j < len(subtasks); j++ {
				results[j] = Result{ID: subtasks[j].ID, State: StateSkipped, Err: ctx.Err()}
			}
			wg.Wait()
			return results
		}
		wg.Add(1)
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
// whose goal is set to the subtask's goal. DependsOn is carried verbatim so the
// DAGScheduler can honor the plan's ordering (previously dropped — the deps were
// lost between planning and spawning).
func FromPlan(tree planner.Tree, base summarize.ContextSummary) []Subtask {
	out := make([]Subtask, 0, len(tree.Tasks))
	for _, t := range tree.Tasks {
		s := base
		s.Goal = t.Goal
		if t.Acceptance != "" {
			s.Remaining = "Acceptance: " + t.Acceptance
		}
		// Copy the dependency slice so callers cannot mutate the plan through it.
		var deps []string
		if len(t.DependsOn) > 0 {
			deps = append(deps, t.DependsOn...)
		}
		out = append(out, Subtask{ID: t.ID, Goal: t.Goal, DependsOn: deps, Summary: s})
	}
	return out
}
