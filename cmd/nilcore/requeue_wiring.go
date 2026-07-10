package main

// requeue_wiring.go — Pillar 4 wiring (P11-T23): drive GRANULAR requeue behind
// the NILCORE_REQUEUE + NILCORE_REQUEUE_MAX_ATTEMPTS environment variables (there
// is no front-door flag; the env vars are the whole opt-in surface).
//
// WHY this file exists. internal/requeue is a pure leaf: it turns verifier-set
// claim statuses into a Worklist, plans the MINIMAL focused re-dispatch subtasks,
// and resolves a round's outcome against a bounded retry Ledger — but it invents no
// loop and touches no orchestrator. This file is the seam that hands those decisions
// to the EXISTING machinery: it scans the worktree's artifacts, plans the focused
// subtasks, drives them through the SAME spawn.DAGScheduler the dispatcher uses
// (cutting each retry from the prior attempt via ContinueFrom so passing claims are
// preserved), re-runs the SAME evverify.ArtifactVerifier to get a fresh verdict
// (green is the verifier's, never a stored status — I2), resolves resolved/stillFailed/
// exhausted, persists the Ledger beside agent.RunState in store.Task.Detail, and
// appends the additive claim_requeue / claim_resolved / requeue_exhausted event kinds
// (I5 — the log only grows).
//
// HOW it plugs in. It produces a super.Supervisor.RequeueHook — the exact
// func(ctx)(remaining []string, exhausted bool) signature P11-T22 pinned — consulted
// by the supervisor EXACTLY ONCE at convergence-red. The returned `remaining` unit
// keys are HARNESS-trusted control ids (the verifier set the statuses; this file
// derived the keys) the supervisor folds into its next focused turn; `exhausted`
// trips the bounded-retry termination rail. nil hook ⇒ the supervisor loop is
// byte-identical to the pre-requeue path.
//
// ADDITIVE + OPT-IN (the byte-identical contract). With NILCORE_REQUEUE unset OR
// MaxAttempts==0 the runner builds NO hook (requeueHook returns nil): no scan, no
// dispatch, no re-verify, no store write, no claim_* event — the default binary is
// byte-identical (TestRequeueWiring's unset case asserts this).
//
// Invariants. I1: a requeue is an ordinary spawn dispatch + verifier re-run, no
// backend.go touch. I2: a Unit flips green ONLY because a FRESH ArtifactVerifier
// re-run dropped it from the after-worklist. I3: no secret enters a Unit/Goal/Ledger
// or any claim_* Detail (the keyed re-verify key rides box.ExecWithEnv inside the
// verifier, never this layer). I5: the Ledger (mutable working state) lives in
// store.Task.Detail; every disposition appends a fresh event, none mutate history.
// I7: Units derive from harness-written statuses and the focused Goal is harness-
// authored control text — model-authored claim ids/fields ride as data, never
// instructions.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"nilcore/internal/agent"
	"nilcore/internal/eventlog"
	"nilcore/internal/requeue"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
	"nilcore/internal/store"
	"nilcore/internal/super"
	"nilcore/internal/verify"
)

// requeueEnabled reports whether granular requeue is opted in. The pillar is
// additive and OFF by default: only a non-empty NILCORE_REQUEUE environment
// variable turns it on. Combined with a MaxAttempts>0 budget this is what gates
// every requeue code path; unset ⇒ no hook, byte-identical.
func requeueEnabled() bool {
	return strings.TrimSpace(os.Getenv("NILCORE_REQUEUE")) != ""
}

// requeueMaxAttempts reads the per-Unit retry budget from the
// NILCORE_REQUEUE_MAX_ATTEMPTS environment variable, defaulting to 0. A budget of 0
// disables requeue even when NILCORE_REQUEUE is set (requeue.Ledger reports every
// Unit Exhausted at attempt 0), so the disabled path and the budget-consumed path are
// one — no special-casing. A negative or unparseable value clamps to 0 (fail-safe to
// disabled rather than an unbounded loop).
func requeueMaxAttempts() int {
	raw := strings.TrimSpace(os.Getenv("NILCORE_REQUEUE_MAX_ATTEMPTS"))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// requeueStore is the minimal store surface the runner needs to persist and resume
// the Ledger. It is an interface (not *store.Store directly) so the wiring stays
// hermetically testable with an in-memory fake — the real *store.Store satisfies it.
// A nil store ⇒ the Ledger lives only in memory for the run (no durable resume), which
// keeps requeue usable on a backend without a task store.
type requeueStore interface {
	GetTask(ctx context.Context, id string) (store.Task, error)
	UpsertTask(ctx context.Context, t store.Task) error
}

// requeueDispatch runs one focused requeue subtask in its own worktree and returns
// its result — the SAME super.SpawnFunc shape the dispatcher uses. The wiring site
// supplies the production SpawnFunc (buildSpawnFunc) so requeue reuses the exact
// worktree+verifier+commit pipeline; a test injects a fake that flips an artifact's
// claim to pass to model a successful re-derive. Kept as a func field so this file
// never re-derives spawn plumbing and stays a thin translator.
type requeueDispatch func(ctx context.Context, spec super.SubagentSpec) spawn.Result

// requeueReverify returns a FRESH verify.Verifier over the worktree root — the same
// evverify.ArtifactVerifier the rest of Phase 11 uses, re-run so the after-worklist
// reflects a real re-verification, never a stored status (I2). It is a factory (not a
// cached verifier) because each round must overwrite the on-disk statuses anew. nil ⇒
// the runner skips re-verification and treats the post-dispatch scan as authoritative
// (the dispatch's own per-subagent verifier already governed Passed).
type requeueReverify func() verify.Verifier

// requeueRunner holds everything the RequeueHook needs to run a bounded, focused
// requeue loop without importing the orchestrator into internal/requeue. It is built
// at the wiring site (build.go) from the run's repo root, event log, task store, and
// the production dispatch/verify seams; a test constructs it directly with fakes.
type requeueRunner struct {
	// root is the worktree root the artifacts live under (<root>/.nilcore/artifacts).
	root string
	// role is the role focused subtasks are dispatched under (typed-research — the one
	// that writes evidence artifacts). The Goal already names the red claim ids.
	role roster.Role
	// maxConcurrent caps the focused re-dispatch wave; <1 ⇒ 1.
	maxConcurrent int
	// maxAttempts is the per-Unit retry ceiling (0 ⇒ requeue disabled).
	maxAttempts int

	log      *eventlog.Log
	store    requeueStore
	taskID   string // the store row the Ledger persists under; "" ⇒ in-memory only
	goal     string // carried verbatim into the store row (UpsertTask preserves it)
	dispatch requeueDispatch
	reverify requeueReverify

	// dispatched records the requeue subtask ids run in a PRIOR consultation, so a later
	// round continues each focused fix from its own prior attempt's branch (ContinueFrom =
	// the stable requeue id) rather than re-cutting from base — preserving the claims that
	// already passed across rounds. The requeue id is stable per (artifact,owner), so the
	// same cell continues its own lineage. Single-owner: the hook is consulted on the
	// supervisor's main goroutine, so this map needs no lock.
	dispatched map[string]bool

	// led is the in-memory retry Ledger threaded across consultations within ONE run, so
	// the per-Unit attempt counts survive between hook calls even on a backend without a
	// task store (the bounded-N rail must hold regardless of durable resume). It is seeded
	// once from the store (resume) on the first round and mirrored back on every persist.
	// Single-owner like dispatched.
	led *requeue.Ledger
}

// Hook returns the super.Supervisor.RequeueHook — the func(ctx)(remaining, exhausted)
// pinned by P11-T22 — or nil when requeue is not opted in. A nil hook is the
// byte-identical default: the supervisor's convergence-red path runs exactly as it did
// before Pillar 4. When enabled, the hook performs ONE bounded requeue round per
// convergence-red consultation: scan the failed claims, plan the focused subtasks,
// dispatch them, re-verify, resolve, persist the Ledger, and emit the audit events —
// returning the still-failing unit keys (with budget left) and whether every remaining
// unit is exhausted.
func (r *requeueRunner) Hook() func(ctx context.Context) (remaining []string, exhausted bool) {
	if r == nil || r.maxAttempts <= 0 {
		// Disabled (or no budget): no hook. The supervisor loop stays byte-identical.
		return nil
	}
	return func(ctx context.Context) ([]string, bool) {
		return r.round(ctx)
	}
}

// round runs a single focused requeue round and reports the convergence verdict to the
// supervisor. The flow mirrors the pillar spec exactly:
//
//  1. load the persisted Ledger (resumes attempt counts across runs);
//  2. Scan the worktree → the BEFORE worklist (one Unit per non-pass claim);
//  3. nothing red ⇒ nothing to requeue (remaining empty, not exhausted);
//  4. Plan the minimal focused subtasks (Goal names only red claim ids, ContinueFrom =
//     the prior attempt so passing claims survive) and dispatch them through the
//     existing DAGScheduler;
//  5. re-run the ArtifactVerifier so the AFTER worklist is a fresh verdict (I2);
//  6. Resolve(before, after) → resolved / stillFailed / exhausted, Bumping the Ledger;
//  7. persist the Ledger beside agent.RunState in store.Task.Detail;
//  8. emit claim_resolved / claim_requeue / requeue_exhausted (I5);
//  9. report remaining (still-failing with budget) + whether all remaining are exhausted.
func (r *requeueRunner) round(ctx context.Context) (remaining []string, exhausted bool) {
	led := r.loadLedger(ctx)

	before, err := requeue.Scan(r.root, led)
	if err != nil {
		// A corrupt artifact must not be mistaken for "all green": converge red and stop
		// rather than spin. Reported as exhausted so the supervisor does not loop.
		r.emit("requeue_exhausted", map[string]any{"reason": "scan: " + err.Error()})
		return nil, true
	}
	if len(before.Units) == 0 {
		// Nothing red — there is nothing to requeue. Not exhausted (the convergence-red
		// that triggered the hook is from a non-artifact check); let the supervisor's
		// ordinary loop proceed.
		return nil, false
	}

	// Plan the minimal focused subtasks. The global priorAttempt is "" — each subtask's
	// ContinueFrom is set per-id below to its OWN stable lineage (a cell continues its own
	// prior attempt, not a shared base), which Plan's single-string param cannot express.
	subs := requeue.Plan(before, led, "")
	if len(subs) == 0 {
		// Every red Unit has spent its budget: converge red now (the bounded-retry rail).
		r.emit("requeue_exhausted", map[string]any{"units": unitKeys(before.Units)})
		return nil, true
	}
	// Per-cell continuation: a subtask dispatched in a PRIOR consultation continues from
	// its own prior attempt's branch (ContinueFrom = its stable requeue id), preserving the
	// claims that already passed; a first-seen subtask cuts from base (ContinueFrom "").
	for i := range subs {
		if r.dispatched[subs[i].ID] {
			subs[i].ContinueFrom = subs[i].ID
		}
	}

	// Drive the focused subtasks through the EXISTING DAGScheduler (no new loop). Each
	// requeue.Subtask is translated to a super.SubagentSpec carrying ContinueFrom so the
	// retry worktree is cut from the prior attempt and only the red cells are re-derived.
	r.dispatchSubtasks(ctx, subs)

	// Re-verify so the AFTER worklist is a fresh verdict, never a stored status (I2). A
	// nil reverify factory means the dispatch's own per-subagent verifier already wrote
	// the verdicts back; the post-dispatch Scan then reads those.
	if r.reverify != nil {
		_, _ = r.reverify().Check(ctx)
	}

	after, err := requeue.Scan(r.root, led)
	if err != nil {
		r.emit("requeue_exhausted", map[string]any{"reason": "rescan: " + err.Error()})
		return nil, true
	}

	resolved, stillFailed, exhaustedUnits := requeue.Resolve(before, after, led)
	r.persistLedger(ctx, led)

	// Audit (I5): one append per disposition change — resolved cells, still-failing
	// cells, and the exhausted set. Detail carries ONLY harness-trusted keys (unit key,
	// status, attempt) — no model-authored value/source_url, no secret (I3).
	for _, u := range resolved {
		r.emit("claim_resolved", unitDetail(u))
	}
	for _, u := range stillFailed {
		r.emit("claim_requeue", unitDetail(u))
	}
	if len(exhaustedUnits) > 0 {
		r.emit("requeue_exhausted", map[string]any{"units": unitKeys(exhaustedUnits)})
	}

	// remaining = still-failing units that still have budget (the supervisor folds these
	// into its next focused turn). exhausted = no still-failing unit can be retried, so
	// the loop converges red now.
	remaining = withBudget(stillFailed, exhaustedUnits)
	if len(stillFailed) > 0 && !requeue.ShouldContinue(stillFailed, exhaustedUnits) {
		return remaining, true
	}
	return remaining, false
}

// dispatchSubtasks runs the planned focused subtasks through a fresh DAGScheduler whose
// RunSub translates each leaf requeue.Subtask into a super.SubagentSpec (carrying the
// ContinueFrom base cut) and calls the production dispatch. It reuses the scheduler
// verbatim — Pillar 4 invents no loop — and ignores the per-subtask Results here: the
// authoritative verdict comes from the re-verify + after-Scan, never a self-report (I2).
func (r *requeueRunner) dispatchSubtasks(ctx context.Context, subs []requeue.Subtask) {
	if r.dispatch == nil || len(subs) == 0 {
		return
	}
	specByID := make(map[string]super.SubagentSpec, len(subs))
	dagSubs := make([]spawn.Subtask, 0, len(subs))
	for _, s := range subs {
		spec := super.SubagentSpec{
			ID:           s.ID,
			Role:         r.role,
			Goal:         s.Goal,
			ContinueFrom: s.ContinueFrom,
		}
		specByID[s.ID] = spec
		dagSubs = append(dagSubs, spawn.Subtask{ID: s.ID, Goal: s.Goal, DependsOn: s.DependsOn})
	}
	dag := &spawn.DAGScheduler{
		MaxConcurrent: r.concurrency(),
		RunSub: func(rctx context.Context, t spawn.Subtask) spawn.Result {
			res := r.dispatch(rctx, specByID[t.ID])
			res.ID = t.ID
			return res
		},
	}
	_ = dag.Run(ctx, dagSubs)

	// Record these ids so a later consultation continues each cell from its own attempt.
	if r.dispatched == nil {
		r.dispatched = make(map[string]bool, len(subs))
	}
	for _, s := range subs {
		r.dispatched[s.ID] = true
	}
}

// concurrency clamps maxConcurrent to a usable cap (>=1).
func (r *requeueRunner) concurrency() int {
	if r.maxConcurrent < 1 {
		return 1
	}
	return r.maxConcurrent
}

// loadLedger returns the Ledger for this round. The in-memory r.led is authoritative
// once seeded — it threads the attempt counts across consultations within one run (so
// the bounded-N rail holds without a store). On the FIRST round it is seeded from the
// store (durable resume), or a fresh budget-bearing Ledger when there is no store / no
// row / no prior requeue blob. MaxAttempts always comes from the live flag so a changed
// budget takes effect immediately; only the per-Unit attempt counts resume from disk.
func (r *requeueRunner) loadLedger(ctx context.Context) *requeue.Ledger {
	if r.led != nil {
		r.led.MaxAttempts = r.maxAttempts
		return r.led
	}
	led := &requeue.Ledger{MaxAttempts: r.maxAttempts}
	if r.store != nil && r.taskID != "" {
		if t, err := r.store.GetTask(ctx, r.taskID); err == nil {
			if blob := requeueBlob(t.Detail); len(blob) != 0 {
				if prior, perr := requeue.UnmarshalLedger(blob); perr == nil {
					prior.MaxAttempts = r.maxAttempts // the live flag governs the ceiling
					led = prior
				}
				// A corrupt requeue blob resumes disabled-of-history rather than failing the
				// run: the fresh budget still bounds this run's retries.
			}
		}
	}
	r.led = led
	return led
}

// persistLedger writes the Ledger back into store.Task.Detail as an additive sibling of
// the existing run_state object: the Detail is treated as a generic JSON object so the
// agent.RunState keys (tip_sha/nodes) are preserved verbatim and only a `requeue` key is
// added/replaced. A nil store / empty taskID ⇒ in-memory only (no durable resume). A
// marshal/store failure is logged, never fatal — requeue bookkeeping must not crash a run.
func (r *requeueRunner) persistLedger(ctx context.Context, led *requeue.Ledger) {
	if r.store == nil || r.taskID == "" {
		return
	}
	t, err := r.store.GetTask(ctx, r.taskID)
	if err != nil {
		// No existing row: start one carrying just the requeue sibling. Status mirrors the
		// supervise row convention so the resume paths never re-drive it as a native task.
		t = store.Task{ID: r.taskID, Goal: r.goal, Status: agent.SuperviseStatus}
	}
	detail, err := embedRequeue(t.Detail, led)
	if err != nil {
		r.emit("requeue_exhausted", map[string]any{"reason": "persist: " + err.Error()})
		return
	}
	t.Detail = detail
	if t.Goal == "" {
		t.Goal = r.goal
	}
	if err := r.store.UpsertTask(ctx, t); err != nil {
		r.emit("requeue_exhausted", map[string]any{"reason": "upsert: " + err.Error()})
	}
}

// emit appends an additive requeue event (I5). A nil log is a no-op so the runner is
// usable in a log-less context. Detail carries only harness-trusted keys.
func (r *requeueRunner) emit(kind string, detail map[string]any) {
	if r.log == nil {
		return
	}
	r.log.Append(eventlog.Event{Task: r.taskID, Kind: kind, Detail: detail})
}

// --- pure helpers (no receiver; hermetic) -----------------------------------------

// requeueDetailKey is the sibling key the Ledger lives under inside store.Task.Detail,
// beside agent.RunState's tip_sha/nodes. Distinct from any RunState field so the two
// never collide.
const requeueDetailKey = "requeue"

// requeueBlob extracts the raw requeue Ledger JSON from a store.Task.Detail object, or
// nil when Detail is empty / not an object / has no requeue sibling. It never errors:
// an absent or unparseable Detail simply yields no prior Ledger (resume disabled).
func requeueBlob(detail string) []byte {
	if strings.TrimSpace(detail) == "" {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(detail), &obj); err != nil {
		return nil
	}
	raw, ok := obj[requeueDetailKey]
	if !ok {
		return nil
	}
	return raw
}

// embedRequeue inserts (or replaces) the requeue Ledger as a sibling key in the Detail
// JSON object, preserving every existing agent.RunState key verbatim (forward-compatible:
// unknown keys survive because we round-trip through map[string]json.RawMessage). An
// empty/zero Detail starts a fresh object holding only the requeue sibling.
func embedRequeue(detail string, led *requeue.Ledger) (string, error) {
	obj := map[string]json.RawMessage{}
	if strings.TrimSpace(detail) != "" {
		if err := json.Unmarshal([]byte(detail), &obj); err != nil {
			// A non-object Detail (shouldn't happen — RunState marshals an object) is
			// replaced rather than corrupting the run; start clean with just requeue.
			obj = map[string]json.RawMessage{}
		}
	}
	blob, err := led.Marshal()
	if err != nil {
		return "", fmt.Errorf("requeue: embed ledger: %w", err)
	}
	obj[requeueDetailKey] = json.RawMessage(blob)
	out, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("requeue: marshal detail: %w", err)
	}
	return string(out), nil
}

// unitDetail renders a Unit as a harness-trusted event Detail: ONLY the stable unit key,
// the verifier-set status, and the attempt count. It deliberately omits the model-authored
// value/source_url (never logged) and any secret (I3/I7).
func unitDetail(u requeue.Unit) map[string]any {
	return map[string]any{
		"unit":     u.ArtifactID + "/" + u.ClaimID,
		"claim_id": u.ClaimID,
		"field":    u.Field,
		"status":   string(u.Status),
		"attempt":  u.Attempt,
	}
}

// unitKeys returns the stable ArtifactID/ClaimID keys of a Unit slice, sorted for a
// deterministic event Detail and a stable `remaining` list the supervisor folds in.
func unitKeys(units []requeue.Unit) []string {
	keys := make([]string, 0, len(units))
	for _, u := range units {
		keys = append(keys, u.ArtifactID+"/"+u.ClaimID)
	}
	sort.Strings(keys)
	return keys
}

// withBudget returns the keys of the still-failing units that are NOT exhausted — the
// ones the supervisor's next focused round can still retry. Exhausted units are removed
// so a converged-red cell is never re-offered as "remaining".
func withBudget(stillFailed, exhausted []requeue.Unit) []string {
	ex := make(map[string]struct{}, len(exhausted))
	for _, u := range exhausted {
		ex[u.ArtifactID+"/"+u.ClaimID] = struct{}{}
	}
	out := make([]string, 0, len(stillFailed))
	for _, u := range stillFailed {
		k := u.ArtifactID + "/" + u.ClaimID
		if _, gone := ex[k]; gone {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
