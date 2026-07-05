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
//   1. converged — Scan returns an empty Worklist AND every verifier-green branch is
//                  folded into the tip. The only GREEN exit. An empty worklist with
//                  green work stranded OFF the tip (a merge conflict past its rebuild
//                  budget) exits `unmerged` instead — RED, so a dropped merge can
//                  never masquerade as Done (I2).
//   2. hard-cap  — the ABSOLUTE HardMaxPasses backstop (always on, even UntilClean):
//                  a claim-id-rotating worker that defeats the per-Unit retry Ledger
//                  cannot spin forever burning the budget.
//   3. budget    — the shared ledger's live Total() has reached the GlobalCeiling
//                  (read NON-RECORDINGLY, no probe charge): stop, no shard can make
//                  progress. The global ceiling is a termination rail, never a done-signal.
//   4. exhausted — every still-red Unit has spent its Ledger budget: no shard is
//                  eligible to requeue, so the run converges RED.
//   5. stalled   — two consecutive passes resolved zero NEW units (the worklist did
//                  not shrink): a no-progress loop is stopped RED rather than spun.
//   6. passes    — !UntilClean and the pass count reached MaxPasses: the operator
//                  capped the work; remaining red is reported, not retried.
//   7. ctx       — the context was cancelled/deadlined mid-loop.
//   8. error     — a store/integrate fault aborted the loop (returned as the error).
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

// effectiveHardMaxPasses is the ABSOLUTE pass backstop, enforced on every run including
// an UntilClean one: HardMaxPasses when set, else defaultHardMaxPasses. It is the rail
// that stops a claim-id-rotating worker (one that defeats the per-Unit retry Ledger by
// minting fresh claim ids each pass) from spinning forever and burning the dollar
// budget — the exhausted rail alone would never fire for such a worker.
func (c *Controller) effectiveHardMaxPasses() int {
	if c.HardMaxPasses > 0 {
		return c.HardMaxPasses
	}
	return defaultHardMaxPasses
}

// IntegrateFunc folds the passing shard branches into one verified integration tip,
// rooted at baseRef. baseRef is the PRIOR pass's verified tip (st.TipSHA): each pass
// folds its newly-green branches ONTO that tip instead of re-merging the whole
// accumulated green set from base HEAD, so the persisted TipSHA is load-bearing (MAJOR
// #6) — and the tip stays verifier-green throughout (I2: the integrator re-verifies
// every merge). An empty baseRef means "from HEAD" (the first pass, or a collate run).
// A nil IntegrateFunc means "do not integrate" — the collate presets (research
// dossiers) keep each shard's artifact independent and never merge code, so they pass
// Integrate=nil.
//
// It takes baseRef where the sibling super.IntegrateFunc does not, because the swarm's
// multi-pass loop must re-root each pass on the prior verified tip; the cmd wiring
// supplies a swarm-specific adapter that pins the Integrator's BaseRef to baseRef before
// each fold (cmd/nilcore/swarm.go), rather than reusing the base-ref-less build.go closure.
type IntegrateFunc func(ctx context.Context, baseRef string, order []integrate.MergeItem) (branch string, results []integrate.MergeResult, err error)

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
// passes/ctx/error/unmerged/…). Passes is how many passes ran; TipBranch is the
// integration tip to surface as a PromoteToBase candidate (NEVER auto-landed);
// Remaining is the count of shards not green at exit PLUS any verifier-green shard
// whose branch never reached the tip (a dropped merge is not "done" — I2); Unmerged
// breaks that second component out; Board is the final pass's Scoreboard.
type Outcome struct {
	Done      bool
	Reason    string
	Passes    int
	TipBranch string
	Remaining int
	Unmerged  int
	Board     Scoreboard
}

// Reason constants — the closed set Outcome.Reason draws from, so a caller switches
// exhaustively. converged is the only one paired with Done=true.
const (
	ReasonConverged = "converged" // empty worklist — the green exit
	ReasonExhausted = "exhausted" // every still-red Unit spent its retry budget
	ReasonBudget    = "budget"    // global ceiling: no headroom to continue
	ReasonPasses    = "passes"    // MaxPasses reached with red remaining
	ReasonHardCap   = "hard-cap"  // the absolute HardMaxPasses backstop tripped
	ReasonStalled   = "stalled"   // two consecutive passes made no progress (no new units resolved)
	ReasonCtx       = "ctx"       // context cancelled/deadlined
	ReasonError     = "error"     // a store/integrate fault aborted the loop
	ReasonUnmerged  = "unmerged"  // verifier-green work stayed off the tip past its rebuild budget
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

	// OnPass, if set, is called once at the end of every completed pass (after the
	// pass's verdicts are folded and persisted). The cmd wiring uses it to emit a
	// scoreboard_snapshot to the log so a replayed report reconstructs the same
	// pass-by-pass scoreboard the live render showed (the "live == replay" contract).
	// nil = no-op. It must not block meaningfully (it runs in the pass loop).
	OnPass func()

	// GlobalCeiling is the run's hard dollar wall (the same value the wiring passed to
	// Budget.SetGlobalCeiling). The per-pass headroom probe reads it NON-RECORDINGLY:
	// it compares the ledger's live Total() against this ceiling instead of charging a
	// sub-cent probe each pass (which would accrue a residue against a reserved task).
	// 0 (or a nil Budget) means "no ceiling" — the probe never blocks.
	GlobalCeiling float64

	// HardMaxPasses is an absolute backstop on the number of passes, ALWAYS enforced —
	// even under UntilClean. It exists so a pathological worker that rotates its claim
	// ids (so the retry Ledger never recognizes a Unit as exhausted) cannot spin the
	// loop forever burning the dollar budget. 0 means "use the built-in default"
	// (defaultHardMaxPasses); a positive value overrides it.
	HardMaxPasses int

	// noProgress counts consecutive passes that resolved zero NEW red Units (the
	// remaining worklist did not shrink). Two such passes in a row stop the run — a
	// claim-id-rotating worker makes no progress yet keeps every Unit "fresh", which
	// the exhausted rail alone would never catch. It is run-scoped loop state, reset by
	// Run, so a Controller stays reusable across runs.
	noProgress int

	// term is the run-scoped termination-honesty bookkeeping finish reads to fold any
	// UNRESOLVED planned shard (a DAG dependent that was Skipped and never re-ran) into
	// Remaining and force Done=false — so the run can never report Done=true while a
	// planned node was silently dropped (I2). It is set at the top of Run (so a Controller
	// stays reusable) and shared by reference with the loop's own maps.
	term termAccounting
}

// termAccounting is the run's planned-work ledger for the termination-honesty backstop:
// which shards the run committed to (planned), which ran red and exhausted their budget
// (exhaustedFail), and every shard definition ever seen (allShards). finish counts a
// planned shard that is neither passed, merged, nor exhausted-failed as UNRESOLVED and
// refuses a green verdict while any remain (I2 — no false converge over a skipped node).
type termAccounting struct {
	planned       map[string]bool
	exhaustedFail map[string]bool
	allShards     map[string]Shard
	passed        map[string]spawn.Result
	merged        map[string]bool
	// ran records every planned shard that produced a terminal result this run (green
	// OR verifier-red) — i.e. everything requeue.Scan can see on disk. A shard that only
	// ever SKIPPED (its dep failed) is absent from ran, and that absence is exactly what
	// distinguishes the never-executed node board.Remaining is blind to from a red-with-
	// budget node board.Remaining already counts (so unresolvedPlanned never double-counts).
	ran map[string]bool
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
	// and shrinks to the requeue set (failed-with-budget shards, plus any merge-
	// conflicted greens with rebuild budget) on each subsequent pass.
	current := initial
	// deps is the run-wide id->dependency-ids map, captured ONCE from the full initial
	// set. integrateGreen reads it to fold green branches in dependency order: passed
	// accumulates across passes and carries no Deps, and current shrinks each pass, so
	// the complete DAG must come from initial (the only place every shard's Deps live).
	deps := make(map[string][]string, len(initial))
	for i := range initial {
		deps[initial[i].ID] = initial[i].Deps
	}
	// baseGoal pins each shard's ORIGINAL goal so the per-pass suffixes (dep handoff
	// digests, focused-retry evidence, conflict-rebuild instructions) are composed
	// fresh each requeue instead of compounding pass over pass into an unbounded goal.
	baseGoal := make(map[string]string, len(initial))
	for i := range initial {
		baseGoal[initial[i].ID] = initial[i].Goal
	}
	// passed accumulates the shards that have ever gone green, keyed by id, so a later
	// pass never re-runs them (unless their MERGE needs a rebuild) and integration can
	// fold their branches. The Result holds the verified Branch.
	passed := make(map[string]spawn.Result, len(initial))
	// merged is the set of shard ids whose branch is already folded into the verified
	// tip, seeded from the persisted SwarmState (resume) and mirrored back onto
	// st.Merged as integrateGreen lands new folds. It is what scopes each pass's fold
	// to the NOT-yet-merged greens and what the termination-honesty accounting reads.
	merged := make(map[string]bool, len(st.Merged))
	for _, id := range st.Merged {
		merged[id] = true
	}
	// conflictStale marks a green shard whose CURRENT branch already conflicted (or
	// red-combined) on merge: re-merging that same branch would fail identically and
	// spam integration events, so it is excluded from the fold until the shard re-runs
	// and produces a FRESH branch (the fold criterion; run-scoped in-memory state).
	conflictStale := make(map[string]bool)

	// allShards is every shard the run ever saw (the initial set plus any conflict/
	// dependent re-includes), keyed by id, so the termination backstop and the
	// dependent re-inclusion can recover a full Shard definition (Deps/Kind/Pack/Role)
	// for a node that is NOT in the current pass's slice. It is the run's shard registry.
	allShards := make(map[string]Shard, len(initial))
	for i := range initial {
		allShards[initial[i].ID] = initial[i]
	}
	// planned is the id set of every shard from the INITIAL set — the work the run
	// committed to. TERMINATION HONESTY (I2): the run may not report Done=true while any
	// planned shard is neither merged nor terminally-exhausted-failed. A DAG dependent
	// that was Skipped (its dep failed) writes no artifact, so requeue.Scan is blind to
	// it; without this set a run could converge on an empty worklist having NEVER run a
	// planned node. exhaustedFail records planned shards that ran, verified RED, and spent
	// their retry budget — the only honest way a planned node leaves the run un-merged.
	planned := make(map[string]bool, len(initial))
	for i := range initial {
		planned[initial[i].ID] = true
	}
	exhaustedFail := make(map[string]bool)

	// Share the accounting maps (by reference) with finish's termination-honesty backstop
	// so it sees every mutation the loop makes without threading them through finish's
	// signature at each of its many call sites.
	ran := make(map[string]bool)
	c.term = termAccounting{
		planned: planned, exhaustedFail: exhaustedFail, allShards: allShards,
		passed: passed, merged: merged, ran: ran,
	}

	var board Scoreboard

	// prevRemaining anchors the no-progress detector: a pass that leaves the remaining
	// worklist no smaller than the prior pass resolved zero NEW units. -1 is the
	// "no prior pass" sentinel so the FIRST pass can never be counted as a stall.
	prevRemaining := -1
	// prevMerged / prevRan anchor the OTHER forward-progress signals a deep DAG chain
	// makes even when board.Remaining does not shrink: a pass that FOLDED a new branch
	// onto the tip (len(merged) grew) or RAN a previously-skipped node (len(ran) grew)
	// is legitimate progress, not a stall — a chain A←B←C converges one link per pass
	// while the still-skipped tail keeps board.Remaining flat. Without these a slowly-
	// converging chain trips ReasonStalled after two such passes even though it is
	// advancing every pass. Tracking the cumulative counts (which only grow) lets the
	// detector reset on any of the three progress kinds.
	prevMerged, prevRan := 0, 0
	c.noProgress = 0

	for {
		// Honor cancellation at the top of every pass so a deadline between passes stops
		// the loop with a Skipped-style outcome rather than dispatching another wave.
		if err := ctx.Err(); err != nil {
			return c.finish(ctx, st, board, passed, merged, ReasonCtx, false), nil
		}

		// Hard backstop BEFORE dispatch, enforced even under UntilClean: if the run has
		// already spent the absolute pass budget, stop. This is the rail that bounds a
		// claim-id-rotating worker — one whose every red Unit reads "fresh" to the retry
		// Ledger, so the exhausted rail never fires — from looping forever (MINOR #10).
		// It also bounds a permanently-conflicting rebuild ping-pong past the merge
		// Ledger (belt and braces).
		if st.Pass >= c.effectiveHardMaxPasses() {
			return c.finish(ctx, st, board, passed, merged, ReasonHardCap, false), nil
		}

		// Budget rail BEFORE dispatch: if the global ceiling has no headroom there is no
		// point running a pass. We probe with the shared ledger; a global breach stops
		// the run (ErrCeiling is a termination rail, never a done signal).
		if c.globalBudgetExhausted(ctx) {
			return c.finish(ctx, st, board, passed, merged, ReasonBudget, false), nil
		}

		st.Pass++
		board = Scoreboard{Pass: st.Pass}

		// --- run the open shards under the pool; the Fn writes+verifies each artifact.
		// Dispatch a PREPARED COPY: cross-pass dep handoff (BaseRef + fenced digest) is
		// resolved onto the copies so the canonical goals in `current` never accrete;
		// same-pass deps are resolved intra-pass by the Runner's DAG path. ---
		dispatch := prepareShards(current, passed, st.TipSHA)
		flat := !hasDeps(dispatch)
		results := c.Runner.RunPass(ctx, dispatch, flat)

		// Fold this pass's verdicts: mark each shard durably from the VERIFIER verdict
		// (Passed in the Result, set by the Fn's ship gate — I2), accumulate passed
		// shards, and tally the board. RetryPass counts a shard that was red before and
		// is green now (st.Pass>1 guards a first-pass green from being miscounted).
		for i := range current {
			s := current[i]
			res := results[s.ID]
			green := res.Passed && res.Err == nil
			// A DAG dependent whose dependency failed is SKIPPED by the scheduler: it never
			// ran, wrote no artifact, and was NOT verified red — so it must NOT be counted as
			// a failure (that would spend the board's Failed tally on work that never
			// happened) and must NOT be marked ShardFailed. It is tracked as skipped so the
			// dependent re-inclusion (below) and the termination backstop can re-run it once
			// its dep greens (I2: a skipped planned node is not "done").
			skipped := res.State == spawn.StateSkipped
			_, wasPassed := passed[s.ID]
			if !skipped {
				ran[s.ID] = true // produced a terminal verdict ⇒ Scan can see it (see termAccounting.ran)
			}

			board.Checked++
			switch {
			case green:
				board.Passed++
				if st.Pass > 1 && !wasPassed {
					board.RetryPass++ // red in a prior pass, green now
				}
				s.State = ShardPassed
				s.Branch = res.Branch
				passed[s.ID] = res
				// A FRESH green result replaces passed[s.ID] with a NEW branch that has not
				// been merge-attempted yet, so any stale conflict verdict against the shard's
				// PRIOR branch is now moot: clear it so integrateGreen folds the fresh branch.
				// This delete belongs ONLY here (Fix #9): on a RED/SKIPPED re-run of a
				// conflict-rebuild shard, passed[s.ID] STILL holds the OLD green result with
				// the rolled-back branch — clearing the stale mark there would resurrect that
				// known-conflicting branch for a doomed re-merge, burning the merge Ledger.
				delete(conflictStale, s.ID)
			case skipped:
				// Never ran (dep failed): a re-runnable non-terminal state, not a red verdict.
				s.State = ShardSkipped
			default:
				board.Failed++
				s.State = ShardFailed
				// KEEP the preserved failed-attempt branch (the shardFn commits the red
				// WIP so a retry can continue from it): a requeue cuts its worktree from
				// this branch instead of blindly re-rolling from base. An empty Branch
				// (nothing committed) keeps any earlier attempt's branch. The branch is
				// NEVER integrated or used as a dep base — those gate on Passed (I2).
				if res.Branch != "" {
					s.Branch = res.Branch
				}
			}
			s.Attempt = st.Pass - 1
			current[i] = s      // write back: requeueSet reads Branch/State from here
			allShards[s.ID] = s // keep the run's shard registry current
			if err := c.Queue.Mark(ctx, s); err != nil {
				return c.finish(ctx, st, board, passed, merged, ReasonError, false), err
			}
		}

		// --- ask the ARTIFACTS what is still red (I2): Scan derives Units from the
		// verifier-set statuses on disk, never from a worker self-report. ---
		after, err := requeue.Scan(c.Worktree, &st.Ledger)
		if err != nil {
			return c.finish(ctx, st, board, passed, merged, ReasonError, false), err
		}
		board.Remaining = distinctArtifacts(after)

		// Integrate the NOT-yet-merged green branches onto the prior tip (BaseRef=
		// TipSHA), thread the new tip forward, and learn which folds were rolled back
		// (conflict / red-combined). Collate presets pass Integrate=nil and skip this.
		conflicted, ierr := c.integrateGreen(ctx, passed, deps, merged, conflictStale, &st)
		if ierr != nil {
			return c.finish(ctx, st, board, passed, merged, ReasonError, false), ierr
		}

		// Conflict requeue: a shard that greened SOLO but could not be folded is NOT
		// silently dropped (I2 — verified work must reach the tip or surface). Each
		// conflict spends one merge-rebuild attempt against the retry Ledger (bounded —
		// no infinite rebuild loop); shards with budget re-enter the next pass cut from
		// the integrated tip with a harness-authored rebuild goal. Computed BEFORE
		// SaveState so the spent attempts persist with this pass's snapshot.
		conflictRetry := c.conflictRequeue(current, conflicted, baseGoal, &st)

		// Persist the run row once per pass (crash-atomic): Pass/Ledger/TipSHA/Merged
		// reflect this completed pass, so a resume re-Scans from a consistent snapshot.
		if err := c.Queue.SaveState(ctx, st); err != nil {
			return c.finish(ctx, st, board, passed, merged, ReasonError, false), err
		}

		// Poll the audit chain each pass: a broken log HALTS the run (I5) — a swarm must
		// not keep shipping verdicts onto a tampered trail.
		if c.Log != nil {
			if lerr := c.Log.Err(); lerr != nil {
				return c.finish(ctx, st, board, passed, merged, ReasonError, false), lerr
			}
		}

		// Emit the per-pass scoreboard snapshot (live == replay): without this the only
		// emitter of scoreboard_snapshot events never ran in production, so a replayed
		// swarm report came out all-zero.
		if c.OnPass != nil {
			c.OnPass()
		}

		// --- convergence: an empty worklist AND no green work stranded off the tip ---
		if len(after.Units) == 0 {
			// A skipped DAG dependent whose dep has since greened is INVISIBLE to Scan (it
			// wrote no artifact), so an empty worklist does NOT prove the planned work ran.
			// Re-run every planned shard still unresolved (skipped, or otherwise never-
			// merged/never-exhausted): the DAG scheduler re-skips it harmlessly if its dep is
			// still red, and runs it once the dep is green. This is what makes A←B (A red pass
			// 1 ⇒ B skipped) re-run B on pass 2 after A greens, instead of a false converge (I2).
			pending := c.pendingPlanned(planned, passed, merged, exhaustedFail, allShards, st.TipSHA)
			if len(conflictRetry) > 0 || len(pending) > 0 {
				// Some verified branch is off the tip with rebuild budget, and/or a planned
				// node never ran: converging now would silently drop work (I2). Run another
				// pass — still under the operator's pass cap (and the hard/budget rails at the
				// loop top). At the cap, surface the honest non-green exit, never a false converge.
				if !c.Policy.UntilClean && st.Pass >= c.effectiveMaxPasses() {
					return c.finish(ctx, st, board, passed, merged, ReasonPasses, false), nil
				}
				current = append(conflictRetry, pending...)
				continue
			}
			if unmergedGreens(c.Integrate != nil, passed, merged) > 0 {
				// TERMINATION HONESTY: green shards that stayed unmerged past their
				// rebuild budget surface as a RED exit — Done stays false and the drop is
				// counted in Remaining — never a Done=true that lost verified work.
				return c.finish(ctx, st, board, passed, merged, ReasonUnmerged, false), nil
			}
			return c.finish(ctx, st, board, passed, merged, ReasonConverged, true), nil
		}

		// No-progress detector: a pass is a stall ONLY if it made NO forward progress of
		// ANY kind. A claim-id-rotating worker keeps the worklist the same size (or larger)
		// forever while every Unit reads "fresh" to the Ledger, so neither the exhausted nor
		// the budget rail would catch it promptly — two consecutive fully-stalled passes stop
		// the run RED. But a legitimately slow, deep DAG chain advances every pass without
		// shrinking board.Remaining, so we count a pass as PROGRESS if ANY of:
		//   - the worklist shrank (board.Remaining < prevRemaining) — a red Unit resolved;
		//   - a new branch folded onto the tip (len(merged) grew) — a green shard integrated;
		//   - a shard went red→green this pass (board.RetryPass > 0);
		//   - a previously-skipped node RAN this pass (len(ran) grew) — the chain advanced.
		// Only when NONE of these fired is the counter bumped, so a converging chain is never
		// mistaken for a stall. The first pass (prevRemaining<0) can never count as a stall.
		progressed := board.Remaining < prevRemaining ||
			len(merged) > prevMerged ||
			board.RetryPass > 0 ||
			len(ran) > prevRan
		if prevRemaining >= 0 && !progressed {
			c.noProgress++
			if c.noProgress >= 2 {
				return c.finish(ctx, st, board, passed, merged, ReasonStalled, false), nil
			}
		} else {
			c.noProgress = 0
		}
		prevRemaining = board.Remaining
		prevMerged = len(merged)
		prevRan = len(ran)

		// Bump the Ledger for the still-red Units and compute the requeue set: the
		// shards whose artifact still has a NON-EXHAUSTED red Unit. A shard with every
		// red Unit exhausted is dropped (no budget); a passed shard contributes none.
		// This bump happens on EVERY non-converged pass — including the last one a cap
		// permits — so the persisted Ledger durably reflects the attempt just spent.
		eligibleIDs := c.bumpAndSelect(after, &st.Ledger)

		// Record which planned shards have now terminally exhausted their retry budget:
		// they have a red Unit in `after` but did NOT survive the bump into eligibleIDs.
		// The termination backstop reads this so a planned shard that ran, verified red,
		// and spent its budget is an HONEST non-green resolution — not a node the empty-
		// worklist path must keep re-running.
		markExhaustedFail(after, eligibleIDs, planned, exhaustedFail)

		// Persist the ledger AGAIN now that bumpAndSelect (above) has spent this pass's
		// red-claim attempts (Fix #12). The pass-boundary SaveState ran BEFORE that bump,
		// so without this second write the bump would only reach disk on the NEXT pass's
		// boundary — and a crash in between (or an exit at the passes/exhausted rail below,
		// which return without a further SaveState) would resume with this pass's spent
		// attempts forgotten and grant extra retries. Persisting HERE, before those exit
		// rails, mirrors conflictRequeue's "spend the attempts, then persist with this
		// pass" discipline. The Pass counter is unchanged, so re-persisting the same
		// snapshot with the fuller ledger is safe and idempotent.
		if err := c.Queue.SaveState(ctx, st); err != nil {
			return c.finish(ctx, st, board, passed, merged, ReasonError, false), err
		}

		// Passes rail FIRST when bounded: if not until-clean and the pass count reached
		// the permitted maximum, the OPERATOR CAP is what stopped the run — report
		// `passes`, not `exhausted`, even if the ledger is also out of budget (the cap is
		// the binding constraint; budget never got a chance to matter). This is also the
		// default-off surface (MaxPasses<=1, !UntilClean): a still-red single-pass run
		// stops here after pass 1 rather than requeuing. UntilClean skips this rail, so an
		// until-clean run falls through to the exhausted check below — which is why the
		// exhausted reason only ever surfaces for an until-clean (or high-MaxPasses) run.
		if !c.Policy.UntilClean && st.Pass >= c.effectiveMaxPasses() {
			return c.finish(ctx, st, board, passed, merged, ReasonPasses, false), nil
		}

		// Exhausted rail: another pass IS permitted, but no red shard retains retry
		// budget AND no conflicted green retains rebuild budget — so the run converges
		// RED rather than dispatching a pass that could change nothing.
		if len(eligibleIDs) == 0 && len(conflictRetry) == 0 {
			return c.finish(ctx, st, board, passed, merged, ReasonExhausted, false), nil
		}

		// Build the next pass's shard set: the still-red shards with budget (their goal
		// re-composed as an EVIDENCE-CARRYING focused retry, their base the preserved
		// failed-attempt branch) plus any merge-conflicted greens rebuilding on the tip.
		// The focused goals read the SAME post-bump Ledger bumpAndSelect wrote, so the
		// plan's exhaustion view matches the eligible set.
		focus := focusedGoals(after, &st.Ledger)
		next := append(c.requeueSet(current, eligibleIDs, baseGoal, focus), conflictRetry...)
		// Also re-include the not-yet-resolved DEPENDENTS of the requeued red set: a shard
		// whose dep is red was Skipped this pass, wrote no artifact, and is invisible to
		// Scan — so requeueSet alone never re-runs it. Adding it back (reset to queued,
		// BaseRef re-resolved by prepareShards next pass) means once its dep greens it
		// actually runs; while the dep stays red the DAG scheduler re-skips it harmlessly.
		requeuedNow := idSet(next)
		current = append(next, c.pendingDependents(requeuedNow, planned, passed, merged, exhaustedFail, allShards)...)
	}
}

// finish builds the terminal Outcome and emits a metadata-only scoreboard event.
// TERMINATION HONESTY: whatever rail stopped the loop, a verifier-green shard whose
// branch never reached the tip is COUNTED — folded into Remaining (and broken out as
// Unmerged) — and can force Done back to false, so a run can never report Done=true
// while verified work was silently dropped from the integration tip (I2).
func (c *Controller) finish(ctx context.Context, st SwarmState, board Scoreboard, passed map[string]spawn.Result, merged map[string]bool, reason string, done bool) Outcome {
	unmerged := unmergedGreens(c.Integrate != nil, passed, merged)
	out := Outcome{
		Done:      done,
		Reason:    reason,
		Passes:    st.Pass,
		TipBranch: st.TipSHA,
		Remaining: board.Remaining + unmerged,
		Unmerged:  unmerged,
		Board:     board,
	}
	// Belt and braces: the loop never passes done=true with unmerged greens, but if a
	// future edit did, the honesty backstop here flips the verdict rather than lying.
	if out.Done && unmerged > 0 {
		out.Done, out.Reason = false, ReasonUnmerged
	}
	// TERMINATION HONESTY for the DAG (Fix #21): count every planned shard that never
	// reached an honest terminal disposition — a dependent that was Skipped (its dep
	// failed) and, for whatever reason (cap/budget/ctx hit first), never re-ran. Such a
	// node wrote no artifact, so board.Remaining (derived from Scan) is blind to it. Fold
	// it into Remaining and force Done=false: the run must never report Done=true/
	// Remaining=0 while a planned node was silently dropped (I2).
	if unresolved := c.unresolvedPlanned(); unresolved > 0 {
		out.Remaining += unresolved
		if out.Done {
			out.Done, out.Reason = false, ReasonUnmerged
		}
	}
	// On a clean converge there is nothing red and nothing stranded; force Remaining to
	// 0 so the Outcome and the Done flag agree even if a late Scan raced an in-flight
	// write.
	if out.Done {
		out.Remaining = 0
		// Move the run row to a TERMINAL status so a later --resume does not re-adopt a
		// finished run and spin a no-op pass: a converged run has nothing red left to
		// re-drive. Only the GREEN converged exit reaches here (every honesty backstop
		// above has already flipped a dishonest Done to false), so a capped/red/exhausted
		// run keeps StatusRun and stays resumable. Best-effort: a durability-write failure
		// is logged but never fails a healthy converged run (resume degrades to re-adopting
		// it, the pre-fix behaviour), mirroring recordIntegration's SaveState discipline.
		if c.Queue != nil {
			if err := c.Queue.MarkConverged(ctx, st); err != nil {
				c.emit("swarm_markconverged_error", map[string]any{"error": err.Error()})
			}
		}
	}
	c.emit("swarm_done", map[string]any{
		"reason": out.Reason, "done": out.Done, "passes": st.Pass,
		"remaining": out.Remaining, "unmerged": unmerged,
		"checked": board.Checked, "passed": board.Passed,
		"failed": board.Failed, "retry_pass": board.RetryPass,
	})
	return out
}

// unresolvedPlanned counts ONLY the planned shards that NEVER RAN — a dependent that
// was Skipped (its dep failed) and, for whatever reason (cap/budget/ctx hit first),
// never re-ran. Such a node wrote no artifact, so board.Remaining (derived from Scan)
// is blind to it; folding it into Remaining is what keeps the run from a false green
// over a silently-dropped DAG node (Fix #21, I2).
//
// It deliberately counts NOTHING that ran: a red-with-budget or exhausted-red shard is
// in `ran` AND has a red artifact board.Remaining already counts, and a verifier-green-
// but-unmerged shard is owned by unmergedGreens — so keying strictly on `!ran` makes
// this counter disjoint from BOTH board.Remaining and unmergedGreens (it previously
// double-counted red-with-budget shards against board.Remaining). A merged shard ran by
// definition; the explicit merged guard is belt-and-braces. Zero value of c.term (no Run
// yet) yields 0, keeping finish safe if ever called without loop setup.
func (c *Controller) unresolvedPlanned() int {
	n := 0
	for id := range c.term.planned {
		if c.term.ran[id] || c.term.merged[id] {
			continue // ran (⇒ Scan-visible / green-owned) or merged ⇒ not a silent drop
		}
		n++ // planned but never executed this run ⇒ a dropped DAG node
	}
	return n
}

// unmergedGreens counts the verifier-green shards whose branch has NOT been folded
// into the integration tip — the work a dishonest terminal report would silently
// drop. Only meaningful when an integrator is wired (a collate preset never merges,
// so nothing can be "unmerged"); a green shard with no Branch (a non-code artifact)
// contributes no MergeItem and therefore does not count.
func unmergedGreens(integrating bool, passed map[string]spawn.Result, merged map[string]bool) int {
	if !integrating {
		return 0
	}
	n := 0
	for id, res := range passed {
		if res.Branch != "" && !merged[id] {
			n++
		}
	}
	return n
}

// planResolved reports whether a planned shard has reached an HONEST terminal
// disposition — merged into the tip, verifier-green (collate presets: green with no
// branch is done; a green-with-branch that is unmerged is handled by unmergedGreens,
// not here), or ran red and exhausted its retry budget. A shard that is none of these
// (Skipped because its dep failed, or otherwise never-run) is UNRESOLVED: the run may
// not converge Done=true while it is outstanding (I2 — never a false green).
func planResolved(id string, passed map[string]spawn.Result, merged, exhaustedFail map[string]bool) bool {
	if merged[id] || exhaustedFail[id] {
		return true
	}
	if _, ok := passed[id]; ok {
		return true // verifier-green (unmerged-with-branch is caught separately)
	}
	return false
}

// pendingPlanned returns the not-yet-resolved planned shards, freshly reset to
// ShardQueued with BaseRef cleared so prepareShards re-resolves their dep base next
// pass. It is called on the EMPTY-worklist path: a skipped dependent wrote no
// artifact, so Scan cannot see it — this is the only place such a node re-enters the
// loop. The order follows the id sort so the re-run set is stable. tipSHA is unused
// today (prepareShards re-resolves from passed/tip on dispatch) but kept in the
// signature so a future direct base-resolve has it to hand.
func (c *Controller) pendingPlanned(planned map[string]bool, passed map[string]spawn.Result, merged, exhaustedFail map[string]bool, allShards map[string]Shard, tipSHA string) []Shard {
	_ = tipSHA
	var out []Shard
	for _, id := range sortedIDs(planned) {
		if planResolved(id, passed, merged, exhaustedFail) {
			continue
		}
		if s, ok := allShards[id]; ok {
			out = append(out, resetToQueued(s))
		}
	}
	return out
}

// pendingDependents returns the not-yet-resolved planned shards whose Deps intersect
// the set of shards already being requeued this pass (requeuedNow), excluding any
// already in that set. These are the DAG dependents that were Skipped when their dep
// went red: adding them back (reset to queued) means they actually run once the dep
// greens, while the DAG scheduler re-skips them harmlessly if the dep is still red.
// Without this, a skipped dependent — invisible to Scan — would never re-run and the
// run could converge missing its planned work (I2).
func (c *Controller) pendingDependents(requeuedNow, planned map[string]bool, passed map[string]spawn.Result, merged, exhaustedFail map[string]bool, allShards map[string]Shard) []Shard {
	var out []Shard
	for _, id := range sortedIDs(planned) {
		if requeuedNow[id] || planResolved(id, passed, merged, exhaustedFail) {
			continue
		}
		s, ok := allShards[id]
		if !ok {
			continue
		}
		depOnRequeued := false
		for _, dep := range s.Deps {
			if requeuedNow[dep] {
				depOnRequeued = true
				break
			}
		}
		if depOnRequeued {
			out = append(out, resetToQueued(s))
		}
	}
	return out
}

// resetToQueued returns a copy of s re-armed for another pass: ShardQueued state and
// an empty BaseRef so prepareShards re-resolves its dependency base fresh (the dep's
// verified branch / integrated tip) rather than a stale one. Its Goal is left at the
// canonical value — the Controller re-composes per-pass suffixes on a copy at dispatch.
func resetToQueued(s Shard) Shard {
	s.State = ShardQueued
	s.BaseRef = ""
	return s
}

// markExhaustedFail records, into exhaustedFail, every planned shard that has a red
// Unit in `after` but did NOT survive the ledger bump into eligibleIDs — i.e. it ran,
// verified red, and spent its retry budget. The termination backstop treats such a
// shard as honestly resolved (non-green) rather than a node to keep re-running.
func markExhaustedFail(after requeue.Worklist, eligibleIDs []string, planned, exhaustedFail map[string]bool) {
	eligible := make(map[string]bool, len(eligibleIDs))
	for _, id := range eligibleIDs {
		eligible[id] = true
	}
	for _, u := range after.Units {
		if planned[u.ArtifactID] && !eligible[u.ArtifactID] {
			exhaustedFail[u.ArtifactID] = true
		}
	}
}

// idSet projects a shard slice onto the set of its ids.
func idSet(shards []Shard) map[string]bool {
	m := make(map[string]bool, len(shards))
	for i := range shards {
		m[shards[i].ID] = true
	}
	return m
}

// sortedIDs returns the keys of an id set in lexical order, for deterministic
// re-inclusion ordering (the same stable-emit discipline mergeOrder uses).
func sortedIDs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// integrateGreen folds the NOT-yet-merged green branches through the IntegrateFunc,
// in a DEPENDENCY-RESPECTING order (a node after the deps it was coded on top of),
// with the prior tip as the base. It threads the resulting integration tip onto
// st.TipSHA so the NEXT pass folds remaining green work on top of the already-merged
// tip (no work lost across passes), records every Merged&&Verified fold into the
// merged set + st.Merged (so a later pass never re-merges it — the event-spam fix),
// and marks every rolled-back fold (conflict, or red-combined) conflictStale + returns
// its id so the Controller can requeue a rebuild. A nil Integrate (collate preset) is
// a no-op. A shard with no Branch (verified but not a code branch — e.g. a research
// artifact) contributes no MergeItem. deps is the run-wide id->dependency-ids map
// (passed carries no Deps), so the fold can honor the DAG even though passed
// accumulates across passes.
func (c *Controller) integrateGreen(ctx context.Context, passed map[string]spawn.Result, deps map[string][]string, merged, conflictStale map[string]bool, st *SwarmState) ([]string, error) {
	if c.Integrate == nil {
		return nil, nil // collate preset: artifacts stay independent, never merged
	}
	// Fold ONLY the pending greens: not yet on the tip, and not a branch that already
	// conflicted (a stale branch merges identically until its shard re-runs — retrying
	// it would only spam integration events; the rebuild path mints a fresh branch).
	pending := make(map[string]spawn.Result, len(passed))
	for id, res := range passed {
		if res.Branch == "" || merged[id] || conflictStale[id] {
			continue
		}
		pending[id] = res
	}
	order := mergeOrder(pending, deps)
	if len(order) == 0 {
		return nil, nil // nothing new to fold this pass
	}
	// Fold THIS pass's green branches onto the prior verified tip (st.TipSHA), not base
	// HEAD: the swarm-wiring adapter pins the Integrator's BaseRef to this baseRef before
	// merging, so each pass extends the already-merged work rather than re-deriving it
	// (MAJOR #6). The first pass passes "" ⇒ the integrator starts from HEAD.
	branch, results, err := c.Integrate(ctx, st.TipSHA, order)
	if err != nil {
		return nil, err
	}
	var conflicted []string
	for _, r := range results {
		if r.Merged && r.Verified {
			if !merged[r.ID] {
				merged[r.ID] = true
				st.Merged = append(st.Merged, r.ID) // durable mirror, persisted by SaveState
			}
			continue
		}
		// Conflict or red-combined ⇒ the Integrator rolled this branch back: the tip
		// does NOT contain its verified work. Stale until the shard produces a fresh
		// branch; the Controller decides (under the Ledger) whether to requeue a rebuild.
		conflictStale[r.ID] = true
		conflicted = append(conflicted, r.ID)
	}
	// Advance the tip to the last green-and-verified merge SHA so the next pass cuts
	// from there. The IntegrateFunc returns the integration branch name; the verified
	// SHA threads through the MergeResults (the last Merged && Verified one).
	if sha := lastVerifiedSHA(results); sha != "" {
		st.TipSHA = sha
	}
	c.emit("swarm_integrate", map[string]any{
		"branch": branch, "items": len(order), "tip_sha": st.TipSHA,
		"merged": len(order) - len(conflicted), "conflicts": len(conflicted),
	})
	return conflicted, nil
}

// mergeConflictClaimID is the synthetic claim id a merge-rebuild attempt is budgeted
// under in the run's retry Ledger, keyed "<shardID>/<this>". A merge conflict is not
// an artifact claim, but it consumes the SAME kind of bounded retry a red claim does —
// reusing the Ledger keeps one budget authority and makes the rebuild loop provably
// finite. (A model-authored claim sharing this id would merely share the counter —
// still bounded, never looser.)
const mergeConflictClaimID = "merge-conflict"

// mergeUnit keys a shard's merge-rebuild budget in the requeue Ledger.
func mergeUnit(shardID string) requeue.Unit {
	return requeue.Unit{ArtifactID: shardID, ClaimID: mergeConflictClaimID}
}

// conflictRequeue converts this pass's rolled-back folds into next-pass rebuild
// shards, bounded by the retry Ledger: each conflict spends ONE attempt against the
// shard's synthetic merge Unit; a shard whose merge budget is exhausted is dropped
// from the retry set and left to the termination-honesty accounting (it will surface
// as Unmerged/Remaining — never silently vanish). A retryable shard re-enters queued,
// cut from the CURRENT integrated tip (BaseRef=TipSHA; "" ⇒ HEAD when nothing has
// merged yet) with a harness-authored rebuild goal composed on the shard's ORIGINAL
// goal (no suffix compounding). Shard definitions come from this pass's set — a fold
// is only attempted for a freshly-produced result, so a conflicted shard always ran
// this pass.
func (c *Controller) conflictRequeue(current []Shard, conflicted []string, baseGoal map[string]string, st *SwarmState) []Shard {
	if len(conflicted) == 0 {
		return nil
	}
	byID := make(map[string]Shard, len(current))
	for i := range current {
		byID[current[i].ID] = current[i]
	}
	out := make([]Shard, 0, len(conflicted))
	for _, id := range conflicted {
		u := mergeUnit(id)
		st.Ledger.Bump(u) // one rebuild attempt spent, persisted with this pass
		if st.Ledger.Exhausted(u) {
			c.emit("swarm_merge_exhausted", map[string]any{"shard": id})
			continue // out of rebuild budget: surfaces via unmergedGreens at termination
		}
		s, ok := byID[id]
		if !ok {
			continue // defensive: a fold is only attempted for a shard that ran this pass
		}
		s.State = ShardQueued
		s.Attempt++           // this rebuild is one more attempt at the shard
		s.BaseRef = st.TipSHA // rebuild ON the integrated tip the branch conflicted with
		g := baseGoal[s.ID]
		if g == "" {
			g = s.Goal
		}
		s.Goal = g + "\n\n" + conflictRebuildSuffix
		out = append(out, s)
		c.emit("swarm_conflict_requeue", map[string]any{
			"shard": id, "base": s.BaseRef, "attempt": st.Ledger.Attempts[id+"/"+mergeConflictClaimID],
		})
	}
	return out
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
// with the same routing. Order follows the current slice for determinism. A requeued
// shard is no blind re-roll:
//
//   - its BaseRef is its own preserved failed-attempt branch (continue_from semantics —
//     the fold kept res.Branch on red), so the retry worktree continues the prior work
//     and the claims that already passed survive; no preserved branch ⇒ "" ⇒ base HEAD
//     (or the dep-resolved base prepareShards fills in before dispatch);
//   - its Goal is re-composed from the shard's ORIGINAL goal plus the EVIDENCE-CARRYING
//     focus suffix (red claim ids + verifier Detail), so the retry aims at the exact
//     broken cells. Composing on baseGoal keeps repeated requeues from compounding.
func (c *Controller) requeueSet(current []Shard, eligibleIDs []string, baseGoal, focus map[string]string) []Shard {
	want := make(map[string]bool, len(eligibleIDs))
	for _, id := range eligibleIDs {
		want[id] = true
	}
	out := make([]Shard, 0, len(eligibleIDs))
	for i := range current {
		if want[current[i].ID] {
			s := current[i]
			s.State = ShardQueued // re-queue for the next pass
			s.Attempt++           // the dispatched shard carries the ordinal of the attempt it makes
			s.BaseRef = s.Branch  // continue from the preserved attempt ("" ⇒ HEAD/dep base)
			g := baseGoal[s.ID]
			if g == "" {
				g = s.Goal
			}
			if f := focus[s.ID]; f != "" {
				g += "\n\n" + f
			}
			s.Goal = g
			out = append(out, s)
		}
	}
	return out
}

// globalBudgetExhausted reports whether the shared ledger has run out of global
// headroom, read NON-RECORDINGLY: it compares the ledger's live Total() dollars against
// the run's GlobalCeiling rather than attempting a probe Charge (the old path charged a
// sub-cent budgetProbe against a reserved task EVERY pass, accruing a residue that the
// dollar report then had to explain away). A nil Budget or a non-positive ceiling has no
// wall and never blocks. The epsilon mirrors budget.Ledger's own ceiling tolerance so a
// run that lands EXACTLY on the ceiling is not spuriously stopped a pass early.
func (c *Controller) globalBudgetExhausted(ctx context.Context) bool {
	if c.Budget == nil || c.GlobalCeiling <= 0 {
		return false
	}
	_, spent := c.Budget.Total()
	// No headroom iff the next infinitesimal charge would breach: spent has already
	// reached (within epsilon) the ceiling. Charge refuses when spent+amount >
	// ceiling+epsilon; here we stop one pass BEFORE dispatching when there is no room
	// left for even a token of work, i.e. spent >= ceiling - epsilon.
	return spent >= c.GlobalCeiling-budgetHeadroomEpsilon
}

// emit appends one metadata-only controller event (a nil log is a no-op).
func (c *Controller) emit(kind string, detail map[string]any) {
	if c.Log == nil {
		return
	}
	c.Log.Append(eventlog.Event{Kind: kind, Detail: detail})
}

// budgetHeadroomEpsilon mirrors budget.Ledger's own ceiling tolerance (1e-9) so the
// NON-RECORDING headroom check in globalBudgetExhausted treats a run that landed
// exactly on the ceiling the same way Charge would — no spurious early stop.
const budgetHeadroomEpsilon = 1e-9

// defaultHardMaxPasses is the built-in absolute backstop on the pass count when the
// Controller's HardMaxPasses is unset. It bounds even an UntilClean run so a worker
// that rotates claim ids (defeating the per-Unit retry Ledger) cannot spin forever
// burning the dollar budget. It is generous — a real until-clean run converges in a
// handful of passes — so it only ever trips on a genuinely non-converging loop.
const defaultHardMaxPasses = 50

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

// mergeOrder builds the integration order from the passed shards, in a
// DEPENDENCY-RESPECTING order (a node after every dependency it was coded on top
// of). It mirrors super/dispatch.go's stable topological emit: only passed shards
// with a non-empty Branch are included (a verified artifact with no code branch
// contributes nothing to integrate), and an included shard is emitted only after
// all of its included deps. deps is the id->dependency-ids map for the run's shard
// set (Shard.Deps); passed lacks Deps, so the caller threads it in.
//
// This must NOT lexical-sort shard ids: ids are "swarm-<runID>-<n>" (sharder.go),
// so sort.Strings would put "swarm-run-10" before "swarm-run-2" and could fold a
// dependent before the dependency it builds on. Among ready nodes the emit order
// is shard-id lexical (deterministic fold) — that is safe because ready nodes are
// independent of each other by construction.
func mergeOrder(passed map[string]spawn.Result, deps map[string][]string) []integrate.MergeItem {
	included := make(map[string]bool, len(passed))
	for id := range passed {
		if passed[id].Branch != "" {
			included[id] = true
		}
	}
	order := make([]integrate.MergeItem, 0, len(included))
	emitted := make(map[string]bool, len(included))
	// Bounded passes: each pass emits at least one ready node or stops; with N
	// included nodes the loop runs at most N times (termination by construction).
	for len(emitted) < len(included) {
		// Collect the nodes that are ready THIS pass, then emit them in lexical id
		// order so the fold is deterministic without re-introducing the cross-DAG
		// lexical-before-dependency bug.
		ready := make([]string, 0, len(included))
		for id := range included {
			if emitted[id] {
				continue
			}
			blocked := false
			for _, dep := range deps[id] {
				if included[dep] && !emitted[dep] {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			// A dependency cycle among included nodes: emit the remainder in lexical
			// order rather than spin (the integrator handles conflicts by rollback).
			rest := make([]string, 0, len(included)-len(emitted))
			for id := range included {
				if !emitted[id] {
					rest = append(rest, id)
				}
			}
			sort.Strings(rest)
			for _, id := range rest {
				order = append(order, integrate.MergeItem{ID: id, Branch: passed[id].Branch})
				emitted[id] = true
			}
			break
		}
		sort.Strings(ready)
		for _, id := range ready {
			order = append(order, integrate.MergeItem{ID: id, Branch: passed[id].Branch})
			emitted[id] = true
		}
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
