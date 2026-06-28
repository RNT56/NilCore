// Package route is the adaptive routing policy with the verifier as judge
// (P3-T04): one backend by default; on a hard or failed task, race best-of-N
// backends in parallel worktrees and let the verifier pick the winner; and run a
// cross-model review before the irreversible gate. Every race outcome is logged —
// the data that later earns strength-routing. It implements the agent.Router seam
// structurally (no import of agent), so wiring is additive.
package route

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/verify"
)

// SingleRouter is the default: always the one configured backend.
type SingleRouter struct{}

// Route returns the default backend unchanged.
func (SingleRouter) Route(_ context.Context, _ backend.Task, def backend.CodingBackend) backend.CodingBackend {
	return def
}

// Candidate is one racer: a backend and the verifier for its own isolated
// worktree (the orchestrator builds these in parallel worktrees; route runs and
// judges them).
type Candidate struct {
	Backend  backend.CodingBackend
	Verifier verify.Verifier
	Task     backend.Task

	// Class and Cost are optional routing-learning dimensions (Phase 16, RTE):
	// when set, the race_outcome event carries them so trust.Replay folds the
	// per-(task-class, backend) cost cell. Empty Class / zero Cost ⇒ the event is
	// byte-identical to today (the dimensions are simply absent from Detail).
	Class string
	Cost  float64
}

// Race runs all candidates concurrently and returns the result of the first whose
// verifier passes — the verifier is the judge, so a black-box backend can only
// win by actually making the checks green. ok is false if none passed.
func Race(ctx context.Context, candidates []Candidate, log *eventlog.Log) (backend.Result, bool) {
	type outcome struct {
		res    backend.Result
		passed bool
	}
	results := make([]outcome, len(candidates))
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := candidates[i]
			res, err := c.Backend.Run(ctx, c.Task)
			passed := false
			// Distinguish the three not-passed reasons in the audit trail (I5): a backend
			// run error, a verifier infrastructure error, and a clean verify miss are very
			// different signals for the operator — conflating them all as a silent
			// "passed:false" hides infra failures. The decision is unchanged: only a
			// verifier-green candidate can win (I2), so any error still yields passed=false.
			var failReason string
			if err != nil {
				failReason = "run_error: " + err.Error()
			} else if rep, verr := c.Verifier.Check(ctx); verr != nil {
				failReason = "verify_error: " + verr.Error()
			} else {
				passed = rep.Passed
				if !passed {
					failReason = "verify_failed"
				}
			}
			results[i] = outcome{res, passed}
			detail := map[string]any{"passed": passed}
			if failReason != "" {
				detail["reason"] = failReason
			}
			if c.Class != "" {
				detail["class"] = c.Class
			}
			if c.Cost > 0 {
				detail["cost"] = c.Cost
			}
			log.Append(eventlog.Event{Task: c.Task.ID, Backend: res.Backend, Kind: "race_outcome", Detail: detail})
		}(i)
	}
	wg.Wait()

	for _, o := range results {
		if o.passed {
			return o.res, true
		}
	}
	if len(results) > 0 {
		return results[len(results)-1].res, false
	}
	return backend.Result{}, false
}

const reviewSys = `You are a senior reviewer. Review the proposed change before it ships through an irreversible gate.
Reply with ONLY JSON {"approved": bool, "notes": string}. Approve only if the change is correct, minimal, and safe.`

// Review runs a cross-model review of a change before the irreversible gate. On
// any ambiguity (unparseable output) it denies — the safe default.
func Review(ctx context.Context, reviewer model.Provider, change string) (approved bool, notes string, err error) {
	resp, err := reviewer.Complete(ctx, reviewSys,
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: change}}}}, nil, 1024)
	if err != nil {
		return false, "", err
	}
	text := firstText(resp.Content)
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		var v struct {
			Approved bool   `json:"approved"`
			Notes    string `json:"notes"`
		}
		if json.Unmarshal([]byte(text[start:end+1]), &v) == nil {
			return v.Approved, v.Notes, nil
		}
	}
	return false, text, nil // safe default: deny
}

func firstText(blocks []model.Block) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}
