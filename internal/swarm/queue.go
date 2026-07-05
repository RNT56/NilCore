package swarm

// queue.go — the durable shard queue (SW-T10).
//
// WHY a queue over store + requeue.Ledger. The swarm fans N shards into a bounded
// pool; for "survives restart; requeue only failed shards" to be real, the verdict
// of every shard and the run's retry budget must be DURABLE — not just in-memory
// pool state. The Queue persists each shard as one store.Task row in a swarm-
// DISTINCT Status namespace, and the run's SwarmState (goal, preset, pass counter,
// retry Ledger, integration tip) as one run row. A local-process restart re-reads
// the rows, recomputes the open worklist from the persisted artifacts (the
// Controller's job, SW-T13), and re-drives only the still-red shards.
//
// WHY a swarm-distinct Status namespace. The native orchestrator has its own
// resume sweeps (InFlight over "running", InFlightSupervise over "supervise") that
// re-drive interrupted single/multi-agent tasks. A swarm shard is NOT one of those
// units — it is driven by the Controller, not the native loop — so it MUST NOT
// share a status string with them, or a native sweep would pick up a swarm shard
// and re-run it through the wrong path. The "swarm-*" prefix keeps the two worlds
// disjoint: a native sweep filters on its own statuses and never sees a swarm row,
// and InFlightSwarm filters on "swarm-run" and never sees a native row.
//
// WHY full-Detail read-modify-write on Mark. store.UpsertTask carries Detail
// VERBATIM — a writer that does not set Detail CLEARS it (the store never merges).
// So a status transition that re-wrote only the row's Status would wipe the
// shard's persisted refs/verdict blob. Mark therefore GETs the row, unmarshals the
// existing shardDetail, updates only the changed fields, and writes the FULL record
// back. The single SQLite writer (SetMaxOpenConns(1)) is the serialization point,
// so concurrent Marks from the pool are serialized at the store, not here.
//
// Trust boundary (I2/I5/I7). State is set ONLY from the verifier verdict the caller
// passes (a green report ⇒ ShardPassed; never a self-report). Every emitted event
// is metadata-only (ids, status, attempt, pass) — never the artifact body. The
// shardDetail blob holds REFS + the verifier verdict (Green), never the model-
// authored claim Values.
//
// Resume scope. Resume is LOCAL-PROCESS-RESTART over the LOCAL SQLite store ONLY —
// never cross-host. The store is single-writer on one host; the Queue never opens a
// network transport (asserted by deps_test.go). A multi-host shard queue would be
// EXT-01, explicitly out of this phase.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
	"nilcore/internal/requeue"
	"nilcore/internal/store"
)

// Swarm-distinct Status namespace. These strings are DELIBERATELY prefixed
// "swarm-" so they never collide with the native orchestrator's statuses
// ("running"/"interrupted"/"supervise"): a native resume sweep filters on its own
// status and therefore never re-drives a swarm shard, and InFlightSwarm filters on
// StatusRun and therefore never sees a native task. The shard lifecycle statuses
// mirror the closed ShardState set so a row's Status is the durable projection of
// its in-memory state.
const (
	// StatusRun marks the single run row that carries SwarmState WHILE the run is in
	// flight. It is the only non-terminal run status InFlightSwarm scans for, so an
	// interrupted run is resumable. On a clean converge the row is moved to
	// StatusRunDone (MarkConverged) so a later --resume does NOT re-adopt a finished
	// run and re-drive it (a converged run has nothing red left, so resuming it would
	// spin a no-op pass and mislead the operator).
	StatusRun = "swarm-run"
	// StatusRunDone marks a run row whose loop CONVERGED (the green terminal exit). It
	// is deliberately outside the set InFlightSwarm scans, so a converged run is not
	// discoverable as "interrupted" by --resume. Only a clean converge sets it; a
	// capped/red/exhausted run stays StatusRun (its still-red shards ARE resumable).
	StatusRunDone = "swarm-run-done"
	// StatusQueued is a shard placed on the worklist, not yet dispatched.
	StatusQueued = "swarm-queued"
	// StatusRunning is a shard a worker is currently building.
	StatusRunning = "swarm-running"
	// StatusPassed is a shard whose verifier asserted its artifact green (I2). The
	// ONLY green terminal status.
	StatusPassed = "swarm-passed"
	// StatusFailed is a shard the verifier ran and found not-green — requeue-eligible
	// while its artifact still has a non-exhausted red Unit.
	StatusFailed = "swarm-failed"
	// StatusExhausted is a shard that spent its retry budget and converges red.
	StatusExhausted = "swarm-exhausted"
)

// statusForState projects a closed ShardState onto its durable Status string. It is
// the single mapping point so the queue's namespace and the in-memory enum can
// never drift. A skipped/queued/running shard maps to the matching swarm status; a
// value outside the closed set (impossible by construction) defaults to queued so a
// stray state never lands a row in a native-visible status.
func statusForState(st ShardState) string {
	switch st {
	case ShardRunning:
		return StatusRunning
	case ShardPassed:
		return StatusPassed
	case ShardFailed:
		return StatusFailed
	case ShardExhausted:
		return StatusExhausted
	case ShardQueued, ShardSkipped:
		return StatusQueued
	default:
		return StatusQueued
	}
}

// SwarmState is the durable run-level state, marshaled into the run row's Detail. It
// is the one mutable record a resume reads to re-establish the run: the goal/preset
// it was started with, the pass counter, the retry Ledger (so per-Unit attempt
// budgets survive a restart), and the integration tip SHA (so a resumed integrate
// folds remaining green work ON TOP of the already-merged tip rather than rebuilding
// from base). It holds NO artifact body — only refs and counters.
type SwarmState struct {
	RunID  string         `json:"run_id"`
	Goal   string         `json:"goal"`
	Preset string         `json:"preset,omitempty"`
	Pass   int            `json:"pass"`
	Ledger requeue.Ledger `json:"ledger"`
	TipSHA string         `json:"tip_sha,omitempty"`

	// Merged is the set of shard ids whose verified branch has ALREADY been folded
	// into the integration tip, in fold order. It is what lets integrateGreen fold
	// only the NOT-yet-merged greens each pass (instead of re-merging the whole
	// accumulated green set, spamming integration events) and what makes a green-
	// but-unmerged shard (a merge conflict) DETECTABLE at termination (I2: verified
	// work must never be silently dropped). Backward-compatible: absent in an old
	// persisted blob ⇒ nil ⇒ nothing merged yet, which a resume reconciles by
	// re-attempting the fold (the integrator is idempotent on an already-merged
	// branch: it re-merges cleanly onto the tip that already contains it).
	Merged []string `json:"merged,omitempty"`
}

// shardDetail is one shard's durable Detail blob: REFS + the verifier verdict, never
// the artifact body (I5). Input/Kind/Pack/Role/Deps reconstruct the Shard on resume;
// Attempt/Branch are the runner's bookkeeping; Green is the verifier's verdict for
// the most recent pass (the durable record that a passed shard is NOT re-dispatched).
// It deliberately omits Goal — the Goal lives in the store.Task.Goal column, not in
// Detail, so the row stays greppable and the blob stays refs-only.
type shardDetail struct {
	Input   string   `json:"input,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Pack    string   `json:"pack,omitempty"`
	Role    string   `json:"role,omitempty"`
	Deps    []string `json:"deps,omitempty"`
	Attempt int      `json:"attempt"`
	Branch  string   `json:"branch,omitempty"`
	BaseRef string   `json:"base_ref,omitempty"`
	Green   bool     `json:"green"`
}

// Queue is the durable shard queue for one swarm run. It owns no concurrency of its
// own — the single SQLite writer is the serialization point — and never opens a
// network transport. runID namespaces every row this queue touches (the run row is
// "swarm-<runID>" and each shard ID is "swarm-<runID>-<n>"), so two runs sharing one
// store never read each other's shards.
type Queue struct {
	store *store.Store
	log   *eventlog.Log
	runID string
}

// NewQueue builds a Queue over an open store and the shared audit log for one run.
// The log may be nil (events become no-ops) but the store is required — a Queue with
// no durable backing cannot satisfy its resume contract.
func NewQueue(st *store.Store, log *eventlog.Log, runID string) *Queue {
	return &Queue{store: st, log: log, runID: runID}
}

// runRowID is the run row's task id: "swarm-<runID>". It is distinct from every
// shard id ("swarm-<runID>-<n>") because it has no trailing "-<n>" segment, so
// ShardsByRun's prefix filter excludes it (the prefix is "swarm-<runID>-").
func (q *Queue) runRowID() string { return "swarm-" + q.runID }

// shardPrefix is the ID prefix that identifies a shard of THIS run: "swarm-<runID>-".
// ShardsByRun filters TasksByStatus output by this prefix in Go (no store change), so
// run isolation is a pure string test on the durable id. runID is a fixed-length slug,
// so "swarm-<runID>-" is never a prefix of another run's "swarm-<otherID>-".
func (q *Queue) shardPrefix() string { return "swarm-" + q.runID + "-" }

// Enqueue persists the run row (carrying SwarmState) and one queued row per shard. It
// is idempotent: re-enqueuing a shard already present re-writes its row from the
// shard's current state via the same full-Detail path Mark uses, so a re-plan that
// re-presents a shard does not duplicate or wipe it. The run row is written FIRST so
// a crash between the two leaves a resumable run with its (possibly empty) shard set,
// never orphaned shards with no run state.
func (q *Queue) Enqueue(ctx context.Context, st SwarmState, shards []Shard) error {
	if err := q.SaveState(ctx, st); err != nil {
		return fmt.Errorf("swarm queue: enqueue run row: %w", err)
	}
	for i := range shards {
		if err := q.writeShard(ctx, shards[i]); err != nil {
			return fmt.Errorf("swarm queue: enqueue shard %q: %w", shards[i].ID, err)
		}
		q.emit(shards[i].ID, "shard_queued", map[string]any{
			"status": StatusQueued, "attempt": shards[i].Attempt,
		})
	}
	return nil
}

// SaveState writes the run row in ONE crash-atomic UpsertTask: the SwarmState is
// marshaled into Detail and the row's Goal/Status are set, so a single store write
// either lands the whole run snapshot or none of it. It is the only writer of the run
// row, called once per pass (on the pass boundary) so the persisted Pass/Ledger/
// TipSHA always reflect the last completed pass — never a mid-token partial.
func (q *Queue) SaveState(ctx context.Context, st SwarmState) error {
	detail, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("swarm queue: marshal run state: %w", err)
	}
	if err := q.store.UpsertTask(ctx, store.Task{
		ID:     q.runRowID(),
		Goal:   st.Goal,
		Status: StatusRun,
		Detail: string(detail),
	}); err != nil {
		return fmt.Errorf("swarm queue: save run state: %w", err)
	}
	return nil
}

// MarkConverged moves the run row to the terminal StatusRunDone status while
// preserving its SwarmState Detail (the same crash-atomic full-record write SaveState
// uses), so a converged run is no longer discoverable by InFlightSwarm and a later
// --resume will not re-adopt and re-drive it. It is idempotent (re-marking an already-
// done run rewrites the same terminal row) and a no-op-safe on a run row that was never
// persisted (UpsertTask creates it). Called ONCE by the Controller on the green
// converged exit — every other termination leaves the row at StatusRun so its still-red
// shards stay resumable.
func (q *Queue) MarkConverged(ctx context.Context, st SwarmState) error {
	detail, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("swarm queue: marshal converged run state: %w", err)
	}
	if err := q.store.UpsertTask(ctx, store.Task{
		ID:     q.runRowID(),
		Goal:   st.Goal,
		Status: StatusRunDone,
		Detail: string(detail),
	}); err != nil {
		return fmt.Errorf("swarm queue: mark converged: %w", err)
	}
	return nil
}

// LoadState reads the run row back into a SwarmState for a resume. A missing run row
// (sql.ErrNoRows) is returned verbatim so the caller can distinguish "no such run"
// from a corrupt blob (a hard error). The local store is the only source — never a
// remote one.
func (q *Queue) LoadState(ctx context.Context) (SwarmState, error) {
	row, err := q.store.GetTask(ctx, q.runRowID())
	if err != nil {
		return SwarmState{}, err // includes sql.ErrNoRows verbatim
	}
	var st SwarmState
	if row.Detail != "" {
		if err := json.Unmarshal([]byte(row.Detail), &st); err != nil {
			return SwarmState{}, fmt.Errorf("swarm queue: parse run state: %w", err)
		}
	}
	if st.RunID == "" {
		st.RunID = q.runID
	}
	if st.Goal == "" {
		st.Goal = row.Goal
	}
	return st, nil
}

// Mark records a shard's terminal/lifecycle transition via a FULL-Detail read-modify-
// write so a previously-persisted field (e.g. Green from an earlier pass) is never
// wiped by the overwrite-semantics of UpsertTask. It:
//
//  1. GETs the existing row (sql.ErrNoRows ⇒ the shard was never enqueued — written
//     fresh from the shard's own fields, so Mark is safe to call on a new shard).
//  2. Unmarshals the existing shardDetail.
//  3. Updates only the fields the shard now carries (Attempt/Branch/Green) and the
//     refs, preserving anything the blob already held.
//  4. Writes the FULL record back with the status projected from the shard's State.
//
// State is the basis for the durable Status, and State is set by the caller ONLY from
// the verifier verdict (I2) — Mark never derives green from anything but the State it
// is handed. It emits one metadata-only shard_<state> event (ids/status/attempt only).
func (q *Queue) Mark(ctx context.Context, s Shard) error {
	prev, err := q.store.GetTask(ctx, s.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Never enqueued (or already gone): write a fresh row from the shard. This
		// keeps Mark total — a caller may Mark a shard the queue has not seen.
		if werr := q.writeShard(ctx, s); werr != nil {
			return fmt.Errorf("swarm queue: mark new shard %q: %w", s.ID, werr)
		}
		q.emitMark(s)
		return nil
	case err != nil:
		return fmt.Errorf("swarm queue: read shard %q: %w", s.ID, err)
	}

	// Read-modify-write: start from the PERSISTED blob so a prior field survives.
	var d shardDetail
	if prev.Detail != "" {
		if uerr := json.Unmarshal([]byte(prev.Detail), &d); uerr != nil {
			return fmt.Errorf("swarm queue: parse shard %q detail: %w", s.ID, uerr)
		}
	}
	// Update only what this transition changes. Refs are re-stamped (cheap, stable);
	// Green is carried from the shard's verdict for this pass — a passed shard writes
	// Green=true and a later status re-mark of the SAME shard preserves it because the
	// caller re-presents the green shard (the Controller never re-runs a passed shard).
	d.Input = s.Input
	d.Kind = string(s.Kind)
	d.Pack = s.Pack
	d.Role = s.Role
	d.Deps = s.Deps
	d.Attempt = s.Attempt
	d.Branch = s.Branch
	d.BaseRef = s.BaseRef
	d.Green = (s.State == ShardPassed)

	if err := q.putShard(ctx, s, d); err != nil {
		return err
	}
	q.emitMark(s)
	return nil
}

// writeShard creates/overwrites a shard row directly from the Shard's fields. It is
// used by Enqueue (a fresh queued shard) and by Mark's never-seen branch. The Green
// bit follows the shard's State so a freshly-written passed shard is durably green.
func (q *Queue) writeShard(ctx context.Context, s Shard) error {
	d := shardDetail{
		Input:   s.Input,
		Kind:    string(s.Kind),
		Pack:    s.Pack,
		Role:    s.Role,
		Deps:    s.Deps,
		Attempt: s.Attempt,
		Branch:  s.Branch,
		BaseRef: s.BaseRef,
		Green:   s.State == ShardPassed,
	}
	return q.putShard(ctx, s, d)
}

// putShard marshals d and upserts the shard row with the status projected from the
// shard's State. It is the single store-write point for a shard row, so the Goal
// column and the Detail blob are always written together (full-record write).
func (q *Queue) putShard(ctx context.Context, s Shard, d shardDetail) error {
	blob, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("swarm queue: marshal shard %q detail: %w", s.ID, err)
	}
	status := statusForState(s.State)
	if s.State == "" {
		status = StatusQueued // a zero-state shard is a freshly queued one
	}
	if err := q.store.UpsertTask(ctx, store.Task{
		ID:     s.ID,
		Goal:   s.Goal,
		Status: status,
		Detail: string(blob),
	}); err != nil {
		return fmt.Errorf("swarm queue: write shard %q: %w", s.ID, err)
	}
	return nil
}

// emitMark appends the metadata-only lifecycle event for a Mark, keyed by the shard's
// durable status. The Detail carries ids/status/attempt/branch-presence only — never
// the artifact body or a model-authored Value (I5/I7).
func (q *Queue) emitMark(s Shard) {
	q.emit(s.ID, "shard_"+string(s.State), map[string]any{
		"status":  statusForState(s.State),
		"attempt": s.Attempt,
		"green":   s.State == ShardPassed,
	})
}

// emit appends one metadata-only event to the shared log (a nil log is a no-op). The
// Detail map is caller-built from trusted/metadata fields only.
func (q *Queue) emit(task, kind string, detail map[string]any) {
	if q.log == nil {
		return
	}
	q.log.Append(eventlog.Event{Task: task, Kind: kind, Detail: detail})
}

// ShardsByRun returns every shard row of THIS run, reconstructed into Shards, by
// filtering all swarm-status rows by the "swarm-<runID>-" ID prefix in Go (no store
// query change). It scans each shard status namespace so a run's shards are returned
// regardless of their terminal disposition — the resume path needs the full set to
// recompute which are already green.
func (q *Queue) ShardsByRun(ctx context.Context) ([]Shard, error) {
	var out []Shard
	prefix := q.shardPrefix()
	for _, status := range []string{StatusQueued, StatusRunning, StatusPassed, StatusFailed, StatusExhausted} {
		rows, err := q.store.TasksByStatus(ctx, status)
		if err != nil {
			return nil, fmt.Errorf("swarm queue: list %q: %w", status, err)
		}
		for _, r := range rows {
			if !strings.HasPrefix(r.ID, prefix) {
				continue // a different run's shard sharing the store
			}
			s, err := shardFromRow(r)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// shardFromRow reconstructs a Shard from a persisted store.Task. The State is mapped
// back from the row's Status (the inverse of statusForState), and the refs/verdict
// come from the unmarshaled shardDetail. A corrupt Detail is a hard error — never a
// silently-empty shard that would look "queued, not green".
func shardFromRow(r store.Task) (Shard, error) {
	var d shardDetail
	if r.Detail != "" {
		if err := json.Unmarshal([]byte(r.Detail), &d); err != nil {
			return Shard{}, fmt.Errorf("swarm queue: parse shard %q detail: %w", r.ID, err)
		}
	}
	return Shard{
		ID:      r.ID,
		Goal:    r.Goal,
		Input:   d.Input,
		Kind:    artifactKind(d.Kind),
		Pack:    d.Pack,
		Role:    d.Role,
		Deps:    d.Deps,
		State:   stateForStatus(r.Status),
		Attempt: d.Attempt,
		Branch:  d.Branch,
		BaseRef: d.BaseRef,
	}, nil
}

// Failed returns the swarm-failed shards of this run whose artifact STILL has at
// least one non-exhausted Unit — i.e. the shards eligible for another requeue round.
// It is the durable counterpart to "requeue only failed shards": a shard that failed
// but whose every recorded red Unit is exhausted is NOT returned (no budget left),
// and a passed shard is never returned (it is in a different status namespace, so its
// Fn is never re-invoked).
//
// Eligibility against the Ledger. A shard owns ONE artifact whose id is the shard ID
// (the convention the shardFn writes under), so the shard's Units are the Ledger
// entries keyed "<shardID>/<claimID>". When led is non-nil, a failed shard is
// eligible iff it has at least one such Unit that is NOT yet exhausted. A shard with
// NO recorded Units (a fresh failure the Ledger has not seen) is eligible too — its
// red claims earn their first attempt — UNLESS MaxAttempts==0, the requeue-disabled
// path, where even a first attempt is denied. A nil led skips the Ledger gate
// entirely and returns every failed shard (the Controller then intersects with a
// fresh requeue.Scan, which is the authoritative artifact-level check).
func (q *Queue) Failed(ctx context.Context, led *requeue.Ledger) ([]Shard, error) {
	rows, err := q.store.TasksByStatus(ctx, StatusFailed)
	if err != nil {
		return nil, fmt.Errorf("swarm queue: list failed: %w", err)
	}
	prefix := q.shardPrefix()
	var out []Shard
	for _, r := range rows {
		if !strings.HasPrefix(r.ID, prefix) {
			continue
		}
		s, err := shardFromRow(r)
		if err != nil {
			return nil, err
		}
		if led != nil && !ledgerHasBudget(led, s.ID) {
			continue // every recorded Unit of this shard's artifact is exhausted
		}
		out = append(out, s)
	}
	return out, nil
}

// ledgerHasBudget reports whether the artifact owned by shardID still has retry
// budget: true iff at least one Ledger entry keyed "<shardID>/<claimID>" is below
// MaxAttempts, OR the artifact has no recorded entry at all (a fresh failure earns a
// first attempt) while requeue is enabled (MaxAttempts>0). With MaxAttempts==0
// (requeue disabled) NO shard has budget — the disabled path and the budget-consumed
// path collapse, matching requeue.Exhausted's own semantics.
func ledgerHasBudget(led *requeue.Ledger, shardID string) bool {
	if led.MaxAttempts <= 0 {
		return false // requeue disabled: no shard is eligible
	}
	keyPrefix := shardID + "/"
	sawEntry := false
	for k, attempts := range led.Attempts {
		if !strings.HasPrefix(k, keyPrefix) {
			continue
		}
		sawEntry = true
		if attempts < led.MaxAttempts {
			return true // at least one non-exhausted Unit
		}
	}
	// No entry yet ⇒ a fresh failure with budget remaining; every recorded entry
	// exhausted ⇒ no budget.
	return !sawEntry
}

// InFlightSwarm returns the non-terminal swarm-run rows across the store (every run's
// run row, not just this Queue's), so a resume sweep can discover interrupted swarm
// runs to re-drive. It scans the StatusRun namespace ONLY — the shard lifecycle
// statuses are not "in flight" at the run level — and returns the raw store.Task rows
// so the caller reads each run's SwarmState from Detail.
func (q *Queue) InFlightSwarm(ctx context.Context) ([]store.Task, error) {
	rows, err := q.store.TasksByStatus(ctx, StatusRun)
	if err != nil {
		return nil, fmt.Errorf("swarm queue: in-flight swarm runs: %w", err)
	}
	return rows, nil
}

// artifactKind narrows a persisted Kind string back to the typed artifact.Kind. It
// is a verbatim cast — Kind is descriptive metadata only (it never gates verification),
// so an unrecognized value round-trips as-is rather than being rejected.
func artifactKind(s string) artifact.Kind { return artifact.Kind(s) }

// stateForStatus inverts statusForState: it maps a durable swarm Status back to the
// in-memory ShardState for reconstruction. An unrecognized status (e.g. a native
// row that should never reach here) maps to ShardQueued, the safe non-terminal
// default.
func stateForStatus(status string) ShardState {
	switch status {
	case StatusRunning:
		return ShardRunning
	case StatusPassed:
		return ShardPassed
	case StatusFailed:
		return ShardFailed
	case StatusExhausted:
		return ShardExhausted
	case StatusQueued:
		return ShardQueued
	default:
		return ShardQueued
	}
}
