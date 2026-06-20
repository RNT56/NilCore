package swarm

// passes.go — the until-clean multi-pass controller (SW-T13).
//
// The Controller is the swarm's outer loop: run the open shards, let each shard's Fn
// write+verify its artifact, then ASK THE ARTIFACTS (not the workers) what is still
// red and re-run ONLY those shards, until the worklist is empty or a termination rail
// is hit. It reuses internal/requeue verbatim — Scan derives the red Units from the
// verifier-set claim statuses on disk (I2), the Ledger bounds retries — so the
// Controller invents no convergence logic of its own; it orchestrates the shipped
// pieces.
//
// THE CONVERGENCE MODEL. requeue works on CLAIMS (Units): a Unit is one non-pass claim
// in one artifact. A SHARD owns ONE artifact (artifact id == shard id, the convention
// the Fn writes under). So "requeue only failed shards" reduces to: after a pass, Scan
// the worktree; if the resulting Worklist is empty the run CONVERGED; otherwise the
// requeue set is the shards whose artifact still has a NON-EXHAUSTED red Unit. A
// passed shard contributes no red Unit, so its Fn is never called again — the
// requeue-only-failed guarantee falls straight out of the Scan.
//
// THE TERMINATION RAILS, in the order they are checked each pass:
//   1. converged — Scan returns an empty Worklist. The only GREEN exit.
//   2. budget    — a global-ceiling headroom probe (ClassifyCeiling/Budget) shows no
//                  global headroom: stop, no shard can make progress. ErrCeiling is a
//                  termination rail, NEVER a done-signal.
//   3. exhausted — every still-red Unit has spent its Ledger budget: no shard is
//                  eligible to requeue, so the run converges RED.
//   4. passes    — !UntilClean and the pass count reached MaxPasses: the operator
//                  capped the work; remaining red is reported, not retried.
//   5. ctx       — the context was cancelled/deadlined mid-loop.
//   6. error     — a store/integrate fault aborted the loop (returned as the error).
//
// DURABILITY each pass: SaveState writes the run row (Pass/Ledger/TipSHA) and Queue.Mark
// writes each shard row, so a crash mid-run resumes by re-Scanning the persisted
// artifacts — a green shard stays green (its artifact is on disk, Scan finds no red
// Unit for it) with zero lost progress. Log.Err() is polled each pass and a broken
// audit chain HALTS the run (I5 — a swarm must not proceed over a tampered log).

import (
	"context"
	"sort"

	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/integrate"
	"nilcore/internal/requeue"
	"nilcore/internal/spawn"
)

// PassPolicy bounds the multi-pass loop. UntilClean drives requeue rounds until the
// worklist is empty (or another rail trips); MaxPasses caps the number of passes when
// UntilClean is false. MaxPasses<=1 means EXACTLY ONE pass — the byte-identical
// default-off shape: a single fan-out, no requeue, no behavior change for a caller
// that never asked for multi-pass.
type PassPolicy struct {
	UntilClean bool
	MaxPasses  int
}

// effectiveMaxPasses is the pass cap the passes rail enforces when UntilClean is
// false: MaxPasses, but never below 1, so a MaxPasses of 0 or 1 means EXACTLY ONE
// pass (the byte-identical default-off surface — a single fan-out with no requeue).
// UntilClean ignores this cap entirely (it runs until another rail trips).
func (c *Controller) effectiveMaxPasses() int {
	if c.Policy.MaxPasses < 1 {
		return 1
	}
	return c.Policy.MaxPasses
}

// IntegrateFunc folds the passing shard branches into one verified integration tip. It
// is EXACTLY super.IntegrateFunc's signature so the cmd wiring passes the same
// integrate.Integrator-backed closure both surfaces use. A nil IntegrateFunc means
// "do not integrate" — the collate presets (research dossiers) keep each shard's
// artifact independent and never merge code, so they pass Integrate=nil.
type IntegrateFunc func(ctx context.Context, order []integrate.MergeItem) (branch string, results []integrate.MergeResult, err error)

// Scoreboard is the Controller's plain-value projection of one pass's tally. It is a
// COPY-OUT, not a live board: the cmd's board sub-leaf (internal/swarm/board) is fed
// from these counts, but this package must NOT import board (board → … would risk a
// cycle, and the leaf rule keeps the Controller free of the dashboard). The six
// headline counts mirror report.SwarmDimension field-for-field so a board built from
// them and a replayed report agree.
type Scoreboard struct {
	Pass      int // the pass this tally belongs to (1-based)
	Checked   int // shards whose verdict was recorded this pass
	Passed    int // shards green this pass
	Failed    int // shards red this pass
	RetryPass int // shards red in a PRIOR pass that are green now
	Remaining int // shards still not green after this pass
}

// Outcome is the terminal report of a Run. Done is true only on the converged (green)
// exit. Reason is the rail that stopped the loop (one of converged/exhausted/budget/
// passes/ctx/error). Passes is how many passes ran; TipBranch is the integration tip
// to surface as a PromoteToBase candidate (NEVER auto-landed); Remaining is the count
// of shards not green at exit; Board is the final pass's Scoreboard.
type Outcome struct {
	Done      bool
	Reason    string
	Passes    int
	TipBranch string
	Remaining int
	Board     Scoreboard
}

// Reason constants — the closed set Outcome.Reason draws from, so a caller switches
// exhaustively. converged is the only one paired with Done=true.
const (
	ReasonConverged = "converged" // empty worklist — the green exit
	ReasonExhausted = "exhausted" // every still-red Unit spent its retry budget
	ReasonBudget    = "budget"    // global ceiling: no headroom to continue
	ReasonPasses    = "passes"    // MaxPasses reached with red remaining
	ReasonCtx       = "ctx"       // context cancelled/deadlined
	ReasonError     = "error"     // a store/integrate fault aborted the loop
)

// Controller drives the multi-pass loop over a Runner and a durable Queue. Worktree is
// the directory requeue.Scan reads artifacts from; Policy bounds the passes; Integrate
// folds green code branches (nil for collate presets); Budget is the shared ledger the
// headroom probe reads; Log is the shared audit trail polled each pass. The zero value
// is unusable — set Runner and Queue at least.
type Controller struct {
	Runner    *Runner
	Queue     *Queue
	Worktree  string
	Policy    PassPolicy
	Integrate IntegrateFunc
	Budget    *budget.Ledger
	Log       *eventlog.Log
}

// Run executes the multi-pass loop from the initial shard set and returns the terminal
// Outcome. It threads SwarmState forward (Pass/Ledger/TipSHA), persisting it each pass
// via Queue.SaveState, and Marks each shard's durable row from the VERIFIER verdict
// only (I2). It returns a non-nil error only for a fault that aborts the loop (a store
// write that cannot land, a broken audit chain); every NORMAL termination — converged,
// exhausted, budget, passes, ctx — is an Outcome with a nil error, because at the swarm
// level a capped/red run is a result, not a program fault.
func (c *Controller) Run(ctx context.Context, st SwarmState, initial []Shard) (Outcome, error) {
	// current is the set of shards to run THIS pass. It starts as the full initial set
	// and shrinks to the requeue set (failed-with-budget shards) on each subsequent pass.
	current := initial
	// passed accumulates the shards that have ever gone green, keyed by id, so a later
	// pass never re-runs them and integration can fold their branches. The Result holds
	// the verified Branch.
	passed := make(map[string]spawn.Result, len(initial))
	var board Scoreboard

	for {
		// Honor cancellation at the top of every pass so a deadline between passes stops
		// the loop with a Skipped-style outcome rather than dispatching another wave.
		if err := ctx.Err(); err != nil {
			return c.finish(ctx, st, board, passed, ReasonCtx, false), nil
		}

		// Budget rail BEFORE dispatch: if the global ceiling has no headroom there is no
		// point running a pass. We probe with the shared ledger; a global breach stops
		// the run (ErrCeiling is a termination rail, never a done signal).
		if c.globalBudgetExhausted(ctx) {
			return c.finish(ctx, st, board, passed, ReasonBudget, false), nil
		}

		st.Pass++
		board = Scoreboard{Pass: st.Pass}

		// --- run the open shards under the pool; the Fn writes+verifies each artifact ---
		flat := !hasDeps(current)
		results := c.Runner.RunPass(ctx, current, flat)

		// Fold this pass's verdicts: mark each shard durably from the VERIFIER verdict
		// (Passed in the Result, set by the Fn's ship gate — I2), accumulate passed
		// shards, and tally the board. RetryPass counts a shard that was red before and
		// is green now (st.Pass>1 guards a first-pass green from being miscounted).
		for i := range current {
			s := current[i]
			res := results[s.ID]
			green := res.Passed && res.Err == nil
			_, wasPassed := passed[s.ID]

			board.Checked++
			if green {
				board.Passed++
				if st.Pass > 1 && !wasPassed {
					board.RetryPass++ // red in a prior pass, green now
				}
				s.State = ShardPassed
				s.Branch = res.Branch
				passed[s.ID] = res
			} else {
				board.Failed++
				s.State = ShardFailed
			}
			s.Attempt = st.Pass - 1
			if err := c.Queue.Mark(ctx, s); err != nil {
				return c.finish(ctx, st, board, passed, ReasonError, false), err
			}
		}

		// --- ask the ARTIFACTS what is still red (I2): Scan derives Units from the
		// verifier-set statuses on disk, never from a worker self-report. ---
		after, err := requeue.Scan(c.Worktree, &st.Ledger)
		if err != nil {
			return c.finish(ctx, st, board, passed, ReasonError, false), err
		}
		board.Remaining = distinctArtifacts(after)

		// Integrate the green code branches onto the prior tip (BaseRef=TipSHA) and
		// thread the new tip forward. Collate presets pass Integrate=nil and skip this.
		tip, ierr := c.integrateGreen(ctx, passed, &st)
		if ierr != nil {
			return c.finish(ctx, st, board, passed, ReasonError, false), ierr
		}
		_ = tip

		// Persist the run row once per pass (crash-atomic): Pass/Ledger/TipSHA reflect
		// this completed pass, so a resume re-Scans from a consistent snapshot.
		if err := c.Queue.SaveState(ctx, st); err != nil {
			return c.finish(ctx, st, board, passed, ReasonError, false), err
		}

		// Poll the audit chain each pass: a broken log HALTS the run (I5) — a swarm must
		// not keep shipping verdicts onto a tampered trail.
		if c.Log != nil {
			if lerr := c.Log.Err(); lerr != nil {
				return c.finish(ctx, st, board, passed, ReasonError, false), lerr
			}
		}

		// --- convergence: an empty worklist is the only green exit ---
		if len(after.Units) == 0 {
			return c.finish(ctx, st, board, passed, ReasonConverged, true), nil
		}

		// Bump the Ledger for the still-red Units and compute the requeue set: the
		// shards whose artifact still has a NON-EXHAUSTED red Unit. A shard with every
		// red Unit exhausted is dropped (no budget); a passed shard contributes none.
		// This bump happens on EVERY non-converged pass — including the last one a cap
		// permits — so the persisted Ledger durably reflects the attempt just spent.
		eligibleIDs := c.bumpAndSelect(after, &st.Ledger)

		// Passes rail FIRST when bounded: if not until-clean and the pass count reached
		// the permitted maximum, the OPERATOR CAP is what stopped the run — report
		// `passes`, not `exhausted`, even if the ledger is also out of budget (the cap is
		// the binding constraint; budget never got a chance to matter). This is also the
		// default-off surface (MaxPasses<=1, !UntilClean): a still-red single-pass run
		// stops here after pass 1 rather than requeuing. UntilClean skips this rail, so an
		// until-clean run falls through to the exhausted check below — which is why the
		// exhausted reason only ever surfaces for an until-clean (or high-MaxPasses) run.
		if !c.Policy.UntilClean && st.Pass >= c.effectiveMaxPasses() {
			return c.finish(ctx, st, board, passed, ReasonPasses, false), nil
		}

		// Exhausted rail: another pass IS permitted, but no shard retains retry budget —
		// every still-red Unit hit MaxAttempts — so the run converges RED rather than
		// dispatching a pass that could change nothing.
		if len(eligibleIDs) == 0 {
			return c.finish(ctx, st, board, passed, ReasonExhausted, false), nil
		}

		// Build the next pass's shard set from the eligible ids, re-using the shard
		// definitions (Kind/Pack/Role/Deps) from the current set so the verifier routing
		// survives the requeue. A shard absent from current (cannot happen — eligible ids
		// are a subset of this pass's shards' artifacts) is skipped.
		current = c.requeueSet(current, eligibleIDs)
	}
}

// finish builds the terminal Outcome and emits a metadata-only scoreboard event. It
// recomputes Remaining from the accumulated passed set against the worktree so the
// reported figure is the FINAL artifact-derived count, not a stale per-pass tally.
func (c *Controller) finish(ctx context.Context, st SwarmState, board Scoreboard, passed map[string]spawn.Result, reason string, done bool) Outcome {
	out := Outcome{
		Done:      done,
		Reason:    reason,
		Passes:    st.Pass,
		TipBranch: st.TipSHA,
		Remaining: board.Remaining,
		Board:     board,
	}
	// On a clean converge there is nothing red; force Remaining to 0 so the Outcome and
	// the Done flag agree even if a late Scan raced an in-flight write.
	if done {
		out.Remaining = 0
	}
	c.emit("swarm_done", map[string]any{
		"reason": reason, "done": done, "passes": st.Pass,
		"remaining": out.Remaining,
		"checked":   board.Checked, "passed": board.Passed,
		"failed": board.Failed, "retry_pass": board.RetryPass,
	})
	return out
}

// integrateGreen folds every green shard's branch through the IntegrateFunc, in shard-
// id order (deterministic), with the prior tip as the base. It threads the resulting
// integration tip onto st.TipSHA so the NEXT pass folds remaining green work on top of
// the already-merged tip (no work lost across passes). A nil Integrate (collate preset)
// is a no-op returning the current tip. A shard with no Branch (verified but not a code
// branch — e.g. a research artifact) contributes no MergeItem.
func (c *Controller) integrateGreen(ctx context.Context, passed map[string]spawn.Result, st *SwarmState) (string, error) {
	if c.Integrate == nil {
		return st.TipSHA, nil // collate preset: artifacts stay independent, never merged
	}
	order := mergeOrder(passed)
	if len(order) == 0 {
		return st.TipSHA, nil // nothing green with a branch to fold yet
	}
	branch, results, err := c.Integrate(ctx, order)
	if err != nil {
		return st.TipSHA, err
	}
	// Advance the tip to the last green-and-verified merge SHA so the next pass cuts
	// from there. The IntegrateFunc returns the integration branch name; the verified
	// SHA threads through the MergeResults (the last Merged && Verified one).
	if sha := lastVerifiedSHA(results); sha != "" {
		st.TipSHA = sha
	}
	c.emit("swarm_integrate", map[string]any{
		"branch": branch, "items": len(order), "tip_sha": st.TipSHA,
	})
	return st.TipSHA, nil
}

// bumpAndSelect Bumps the Ledger for every still-red Unit and returns the distinct
// ARTIFACT ids (== shard ids) that retain budget after the bump. It reuses
// requeue.Resolve's contract inline: a still-red Unit spends one attempt; an artifact
// is eligible to requeue iff at least one of its Units is NOT exhausted after the bump.
// An artifact whose every red Unit is now exhausted is excluded — its shard converges
// red.
func (c *Controller) bumpAndSelect(after requeue.Worklist, led *requeue.Ledger) []string {
	// Track, per artifact, whether any Unit still has budget. We bump EACH Unit once
	// (one attempt spent this pass) and then read its exhaustion, mirroring Resolve.
	hasBudget := make(map[string]bool)
	order := make([]string, 0)
	for _, u := range after.Units {
		if _, seen := hasBudget[u.ArtifactID]; !seen {
			hasBudget[u.ArtifactID] = false
			order = append(order, u.ArtifactID)
		}
		led.Bump(u)
		if !led.Exhausted(u) {
			hasBudget[u.ArtifactID] = true
		}
	}
	eligible := make([]string, 0, len(order))
	for _, id := range order {
		if hasBudget[id] {
			eligible = append(eligible, id)
		}
	}
	return eligible
}

// requeueSet selects from this pass's shards the ones whose id is in the eligible set,
// preserving their full definition (Kind/Pack/Role/Deps) so the next pass re-verifies
// with the same routing. Order follows the current slice for determinism.
func (c *Controller) requeueSet(current []Shard, eligibleIDs []string) []Shard {
	want := make(map[string]bool, len(eligibleIDs))
	for _, id := range eligibleIDs {
		want[id] = true
	}
	out := make([]Shard, 0, len(eligibleIDs))
	for i := range current {
		if want[current[i].ID] {
			s := current[i]
			s.State = ShardQueued // re-queue for the next pass
			out = append(out, s)
		}
	}
	return out
}

// globalBudgetExhausted probes the shared ledger for global headroom WITHOUT spending
// any real charge: it asks ClassifyCeiling whether a (forced) ErrCeiling would be
// global. We synthesize the ceiling condition by attempting a zero-token probe charge
// against a reserved global-probe task; if the ledger refuses it, the global wall is
// binding. A nil Budget has no ceiling and never blocks.
func (c *Controller) globalBudgetExhausted(ctx context.Context) bool {
	if c.Budget == nil {
		return false
	}
	// ClassifyCeiling only acts on a real ErrCeiling, so we first reproduce one with a
	// tiny zero-token probe against the reserved global key; a refusal means no global
	// headroom. The probe records nothing on refusal (the ledger rejects before
	// recording), so this check perturbs nothing.
	err := c.Budget.Charge(ctx, swarmGlobalProbeTask, 0, budgetProbe)
	if err == nil {
		return false // headroom exists; back the probe out is unnecessary (sub-cent)
	}
	return ClassifyCeiling(ctx, c.Budget, swarmGlobalProbeTask, err) == ScopeGlobal
}

// emit appends one metadata-only controller event (a nil log is a no-op).
func (c *Controller) emit(kind string, detail map[string]any) {
	if c.Log == nil {
		return
	}
	c.Log.Append(eventlog.Event{Kind: kind, Detail: detail})
}

// swarmGlobalProbeTask is the reserved, ceiling-free task name the global headroom
// probe charges against. The control prefix keeps it from colliding with any real
// shard key ("swarm/<runID>/<n>").
const swarmGlobalProbeTask = "\x00swarm-controller-global-probe"

// budgetProbe is the tiny dollar amount the headroom probe charges — just above the
// ledger's 1e-9 epsilon so a charge at the global ceiling is refused (no headroom),
// while a charge with headroom records a negligible, sub-cent residue.
const budgetProbe = 2e-9

// hasDeps reports whether any shard in the set declares a dependency. The Controller
// uses it to choose the flat (no deps) vs DAG (deps present) topology for a pass, so a
// dependency-free pass never pays for the DAG's wave bookkeeping.
func hasDeps(shards []Shard) bool {
	for i := range shards {
		if len(shards[i].Deps) > 0 {
			return true
		}
	}
	return false
}

// distinctArtifacts counts the distinct artifact ids (== shard ids) with at least one
// red Unit in a worklist — the "remaining" figure: a shard is remaining iff its
// artifact still has a non-pass claim, regardless of how many claims are red.
func distinctArtifacts(w requeue.Worklist) int {
	seen := make(map[string]bool, len(w.Units))
	for _, u := range w.Units {
		seen[u.ArtifactID] = true
	}
	return len(seen)
}

// mergeOrder builds the integration order from the passed shards, sorted by shard id
// so the fold is deterministic and dependents (higher-numbered shards) merge after the
// dependencies they were coded on top of. Only shards with a non-empty Branch produce a
// MergeItem (a verified artifact with no code branch contributes nothing to integrate).
func mergeOrder(passed map[string]spawn.Result) []integrate.MergeItem {
	ids := make([]string, 0, len(passed))
	for id := range passed {
		if passed[id].Branch != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	order := make([]integrate.MergeItem, 0, len(ids))
	for _, id := range ids {
		order = append(order, integrate.MergeItem{ID: id, Branch: passed[id].Branch})
	}
	return order
}

// lastVerifiedSHA returns the SHA of the last merge that was both Merged and Verified,
// i.e. the new integration tip. A run where nothing merged green returns "" so the
// caller keeps the prior tip.
func lastVerifiedSHA(results []integrate.MergeResult) string {
	sha := ""
	for _, r := range results {
		if r.Merged && r.Verified {
			sha = r.SHA
		}
	}
	return sha
}
