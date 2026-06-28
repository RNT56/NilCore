// Package board is the live, O(1)-per-update, concurrency-safe swarm scoreboard
// (Phase 12, SW-T14). One Board value is fed by the runner goroutines as shards are
// queued, dispatched, and verified, and read by a dashboard goroutine that polls
// Snapshot — so every mutator takes a mutex and is constant-time, and Snapshot is an
// immutable copy-out that a concurrent mutator can never tear.
//
// Trust + invariants. Record is the ONLY entry that moves the pass/fail tallies, and
// it is driven STRICTLY by the verifier verdict it is handed (I2): a Board never reads
// a backend self-report, and a fail→pass transition is the one thing that bumps
// RetryPass. MarkClean is the green gate — it only flips FinalCleanPass when the
// worklist is empty AND the caller passes a verified-chain signal, so a green
// scoreboard over a tampered log is impossible. Every event the Board emits is
// metadata only (counts, ids, a pass number) — no model-authored Value/SourceURL
// rides in a Detail (I7) and the Board holds no key (I3). Cost is read LIVE from the
// shared budget.Ledger, so the dollar figure is always the ledger's authoritative
// total, never a number the Board accumulates itself.
//
// Live == replay. The keystone of this task: the same counts the Board exposes
// through Snapshot are written, per pass, into scoreboard_snapshot events, and
// internal/report.ReplaySwarmReport folds those events back into a SwarmDimension. At
// run end Board.Snapshot() must equal ReplaySwarmReport(...).Swarm field-by-field
// (TestLiveVsReplayAgree). That is why EmitSnapshot stamps EXACTLY the six counts
// Snapshot reports and report decodes — the Board and the report agree by construction.
package board

import (
	"sort"
	"sync"
	"time"

	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/meter"
)

// ShardOutcome is the verifier verdict for one shard on one pass — the ONLY input
// that moves the Board's pass/fail tallies. Passed is the VERIFIER's verdict (I2),
// never a backend self-report. Pass is the requeue pass this verdict belongs to (1 on
// the first pass); the Board uses it to roll the per-pass tally and to detect a
// fail→pass retry. Exhausted marks a shard that ran out of requeue attempts and stays
// red. Status/Detail/SourceURL are TRUSTED, metadata-only fields the trace projection
// may surface (SourceURL is key-free provenance per I3); no model-authored Value rides
// here.
type ShardOutcome struct {
	ID        string
	Pass      int
	Passed    bool
	Exhausted bool

	// Trusted, projectable provenance for trace.go. Status/Detail/Verifier are
	// verifier-set; SourceURL is the key-free locator (I3). NONE of these is a
	// model-authored Value (I7) — the Board never carries the asserted datum.
	Verifier  string
	Status    string
	Detail    string
	SourceURL string
}

// shardState is the Board's per-shard bookkeeping. lifecycle tracks where the shard is
// (queued/running/done) so a re-dispatch does not double-count; everFailed remembers a
// prior fail so a later pass can recognize a fail→pass retry; lastPass/passed hold the
// most recent verdict; started anchors the per-shard wall-clock; the trusted-evidence
// fields back the trace projection. No model-authored Value is ever stored.
type shardState struct {
	id         string
	lifecycle  lifecycle
	everFailed bool
	passed     bool
	exhausted  bool
	lastPass   int

	started time.Time
	elapsed time.Duration

	verifier  string
	status    string
	detail    string
	sourceURL string
}

// lifecycle is a shard's coarse position in the run. It guards O(1) idempotence: a
// second MarkQueued/MarkRunning for the same shard does not re-move a count.
type lifecycle uint8

const (
	lifeUnknown lifecycle = iota
	lifeQueued
	lifeRunning
	lifeDone
)

// Board is the concurrency-safe live scoreboard. The zero value is not usable; call
// New. Every method is safe for concurrent use (one mutex guards all mutable state),
// and every mutator is O(1) in the number of shards already seen.
type Board struct {
	mu sync.Mutex

	total   int                    // planned shard count (SetTotal)
	shards  map[string]*shardState // per-shard state, keyed by id
	curPass int                    // highest pass seen by Record (the "current" pass)

	// per-pass tallies — RESET when Record rolls to a higher pass, so each pass's
	// scoreboard_snapshot carries that pass's counts (the PassRow report folds).
	checked   int
	passed    int
	failed    int
	retryPass int

	// per-model token accumulation (OnUsage). Purely observational: it feeds the
	// token line on the rendered scoreboard. The dollar cost is NOT accumulated here —
	// it is read live from the ledger (the ledger is the authority, I2-adjacent).
	usage map[string]tokenPair

	finalCleanPass bool      // set only by MarkClean (the green gate)
	runStart       time.Time // total wall-clock anchor (set by New)

	ledger *budget.Ledger // shared ledger; Cost is read live from Total()
	pricer meter.Pricer   // prices the per-model token line (conservative)

	// EmitSnapshot coalescing: skip an emit if too little time has passed since the
	// last, so a hot inner loop cannot flood the log. lastEmit is the wall-clock of the
	// last emitted snapshot; minInterval is the floor between emits.
	lastEmit    time.Time
	minInterval time.Duration
}

// tokenPair is one model's accumulated input/output token counts.
type tokenPair struct {
	in  int
	out int
}

// New returns a ready Board. ledger is the shared budget.Ledger whose running Total
// the Board reports as Cost (live, never accumulated); pricer prices the per-model
// token line; minInterval is the coalescing floor for EmitSnapshot (a non-positive
// value disables coalescing — every EmitSnapshot writes). A nil ledger reports zero
// cost; a nil pricer prices the token line at zero (the token COUNTS are still shown).
func New(ledger *budget.Ledger, pricer meter.Pricer, minInterval time.Duration) *Board {
	return &Board{
		shards:      map[string]*shardState{},
		usage:       map[string]tokenPair{},
		runStart:    time.Now(),
		ledger:      ledger,
		pricer:      pricer,
		minInterval: minInterval,
	}
}

// SetTotal records the planned shard count. It is display/Remaining context only — it
// never gates a pass. Calling it again overwrites (a re-planned run can grow the set).
func (b *Board) SetTotal(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 {
		n = 0
	}
	b.total = n
}

// MarkQueued records that a shard was placed on the worklist. It is idempotent: a
// shard already known is left in place (a requeue re-queues without double-counting).
// It moves no pass/fail count — only Record does (I2).
func (b *Board) MarkQueued(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.shardLocked(id)
	if s.lifecycle == lifeUnknown {
		s.lifecycle = lifeQueued
	}
}

// MarkRunning records that a shard began running and STAMPS its per-shard start time —
// the source the per-shard wall-clock elapsed is measured from (paired with Record).
// It moves no pass/fail count.
func (b *Board) MarkRunning(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.shardLocked(id)
	s.lifecycle = lifeRunning
	s.started = time.Now()
}

// Record folds one shard's VERIFIER verdict into the tallies. It is the ONLY entry
// that moves passed/failed/retry-pass, and it acts strictly on the verdict it is
// handed (I2) — never on a backend self-report. A verdict whose Pass exceeds the
// current pass rolls the per-pass tally (the prior pass's counts were already
// snapshotted). A fail→pass transition for a shard that everFailed, on a pass after
// the first, bumps RetryPass. It also closes the per-shard wall-clock (elapsed =
// now − started) so the time line has a real source, and stores the trusted evidence
// fields for the trace projection (no model Value).
func (b *Board) Record(o ShardOutcome) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Roll to a new pass when this verdict belongs to a later pass than any seen so
	// far. The prior pass's tallies were emitted as a scoreboard_snapshot before the
	// roll (the runner's contract), so resetting here loses nothing the report needs.
	if o.Pass > b.curPass {
		b.curPass = o.Pass
		b.checked, b.passed, b.failed, b.retryPass = 0, 0, 0, 0
	}

	s := b.shardLocked(o.ID)
	if !s.started.IsZero() {
		s.elapsed = time.Since(s.started)
	}
	s.lifecycle = lifeDone
	s.lastPass = o.Pass
	s.exhausted = o.Exhausted
	s.verifier, s.status, s.detail, s.sourceURL = o.Verifier, o.Status, o.Detail, o.SourceURL

	b.checked++
	if o.Passed {
		b.passed++
		// A shard that previously failed and is now passing on a later pass is a
		// retry-pass — the requeue's whole point. Guard on everFailed AND Pass>1 so a
		// first-pass green is never miscounted as a retry.
		if s.everFailed && o.Pass > 1 {
			b.retryPass++
		}
	} else {
		b.failed++
		s.everFailed = true
	}
	s.passed = o.Passed
}

// OnUsage accumulates one model interaction's token split, keyed by model id. It is
// the seam meter.Provider.OnUsage fans into (per-model in/out tokens) — purely
// observational: it feeds the rendered token line and never affects a tally or the
// cost (cost is the ledger's, read live). Negative counts are clamped so a stray
// negative cannot relax the displayed total.
func (b *Board) OnUsage(modelID string, in, out int) {
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.usage[modelID]
	p.in += in
	p.out += out
	b.usage[modelID] = p
}

// MarkClean is the swarm GREEN gate. It flips FinalCleanPass true ONLY when the
// worklist is empty (emptyWorklist) AND the caller passes a verified-chain signal
// (chainOK) — the same union report's FinalCleanPass requires. A caller must pass
// chainOK=false the moment eventlog.Verify fails, so a green scoreboard can never sit
// over a tampered log (I2/I5). Calling it with either condition false leaves
// FinalCleanPass false.
func (b *Board) MarkClean(emptyWorklist, chainOK bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finalCleanPass = emptyWorklist && chainOK
}

// shardLocked returns the shard's state, creating it on first sight. The caller must
// hold b.mu. It keeps every mutator O(1) — one map lookup, never a scan.
func (b *Board) shardLocked(id string) *shardState {
	s := b.shards[id]
	if s == nil {
		s = &shardState{id: id}
		b.shards[id] = s
	}
	return s
}

// remainingLocked computes the worklist size: the planned total minus the distinct
// shards that have reached a PASS verdict (on any pass). A still-failing or never-run
// shard counts as remaining, so Remaining==0 iff every planned shard is green — the
// condition report's FinalCleanPass also requires. The caller must hold b.mu.
func (b *Board) remainingLocked() int {
	donePass := 0
	for _, s := range b.shards {
		if s.passed {
			donePass++
		}
	}
	rem := b.total - donePass
	if rem < 0 {
		rem = 0
	}
	return rem
}

// --- read side: the immutable Snapshot copy-out + the coalesced EmitSnapshot ---

// Snapshot is an immutable, point-in-time copy of the Board's counts and rollups. It
// is the value every renderer reads. The six headline counts
// (Pass/Checked/Passed/Failed/RetryPass/Remaining) mirror report.SwarmDimension EXACTLY
// so the keystone test can compare field-by-field; Cost is the ledger's live total at
// snapshot time; Tokens/Models back the rendered token line; Shards is the per-shard
// table (trusted metadata only, no model Value); the timers give the rendered time
// line a real source. Every slice/map here is a COPY — mutating a returned Snapshot
// can never reach back into the Board.
type Snapshot struct {
	Pass      int
	Checked   int
	Passed    int
	Failed    int
	RetryPass int
	Remaining int
	Total     int

	FinalCleanPass bool

	// Cost is the shared ledger's live dollar Total at snapshot time (never a number
	// the Board accumulates). Tokens is the summed in+out across every model; Models is
	// the per-model token breakdown, sorted by model id for a deterministic render.
	Cost   float64
	Tokens int
	Models []ModelTokens

	// RunElapsed is total wall-clock since New (the swarm_start→swarm_done span the
	// runner brackets). Shards is the per-shard table in id order.
	RunElapsed time.Duration
	Shards     []ShardRow
}

// ModelTokens is one model's accumulated token split plus its priced dollar cost (via
// the conservative Pricer). The dollar figure here is the PER-MODEL estimate for the
// token line; the AUTHORITATIVE run cost is Snapshot.Cost (the ledger's total).
type ModelTokens struct {
	Model   string
	In      int
	Out     int
	Dollars float64
}

// ShardRow is one shard's row in the scoreboard table: its id, its latest verdict and
// pass, whether it is exhausted, its per-shard wall-clock, and the TRUSTED evidence
// fields (verifier-set Status/Detail/Verifier + the key-free SourceURL). NO
// model-authored Value rides here (I7) — the row carries only what is safe to render.
type ShardRow struct {
	ID        string
	Pass      int
	Passed    bool
	Exhausted bool
	Elapsed   time.Duration

	Verifier  string
	Status    string
	Detail    string
	SourceURL string // key-free provenance (I3) — safe to render as a locator
}

// Snapshot returns an immutable copy of the current Board state. It deep-copies every
// slice and map, so a caller may sort or mutate the returned value with no effect on
// the Board and no data race against a concurrent mutator. Cost is read LIVE from the
// ledger here, so the snapshot's dollar figure is the ledger's authoritative total at
// this instant.
func (b *Board) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

// snapshotLocked builds the copy-out; the caller must hold b.mu. EmitSnapshot reuses
// it so the emitted Detail and a concurrent Snapshot() always agree byte-for-byte.
func (b *Board) snapshotLocked() Snapshot {
	snap := Snapshot{
		Pass:           b.curPass,
		Checked:        b.checked,
		Passed:         b.passed,
		Failed:         b.failed,
		RetryPass:      b.retryPass,
		Remaining:      b.remainingLocked(),
		Total:          b.total,
		FinalCleanPass: b.finalCleanPass,
		RunElapsed:     time.Since(b.runStart),
	}

	// Cost is the ledger's live total — never accumulated by the Board (I2-adjacent:
	// the ledger is the spend authority). A nil ledger reports zero.
	if b.ledger != nil {
		_, dollars := b.ledger.Total()
		snap.Cost = dollars
	}

	// Per-model token breakdown, sorted by model id so the render is deterministic.
	// Each model's dollar figure is the conservative Pricer's estimate; a nil pricer
	// leaves it zero (the token COUNTS still show).
	models := make([]string, 0, len(b.usage))
	for m := range b.usage {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		p := b.usage[m]
		mt := ModelTokens{Model: m, In: p.in, Out: p.out}
		if b.pricer != nil {
			mt.Dollars = b.pricer.Price(m, p.in, p.out)
		}
		snap.Models = append(snap.Models, mt)
		snap.Tokens += p.in + p.out
	}

	// Per-shard table in id order (deterministic). Each row is a copy of trusted,
	// renderable fields only — no model-authored Value.
	ids := make([]string, 0, len(b.shards))
	for id := range b.shards {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := b.shards[id]
		snap.Shards = append(snap.Shards, ShardRow{
			ID:        s.id,
			Pass:      s.lastPass,
			Passed:    s.passed,
			Exhausted: s.exhausted,
			Elapsed:   s.elapsed,
			Verifier:  s.verifier,
			Status:    s.status,
			Detail:    s.detail,
			SourceURL: s.sourceURL,
		})
	}
	return snap
}

// EmitSnapshot appends one scoreboard_snapshot event to log, carrying the current
// pass's six counts as metadata-only Detail (the exact keys report.passRowFromEvent
// decodes — the live==replay contract). It is COALESCED: if fewer than minInterval has
// elapsed since the last emitted snapshot it writes NOTHING and returns false, so a hot
// inner loop cannot flood the log. A non-positive minInterval disables coalescing (it
// always emits). It returns whether an event was written.
//
// The Detail is COUNTS ONLY — no model-authored Value/SourceURL (I7), no secret (I3) —
// so the event is safe to append verbatim and eventlog.Verify stays green. A nil log
// is a no-op (returns false).
func (b *Board) EmitSnapshot(log *eventlog.Log) bool {
	return b.emitSnapshot(log, false)
}

// EmitSnapshotForce emits a snapshot UNCONDITIONALLY (bypassing the minInterval
// coalescing). It is used for the guaranteed FINAL snapshot at run end so the
// replayed report always reflects the same terminal scoreboard the live render
// printed, even when the last pass completed within the coalescing window.
func (b *Board) EmitSnapshotForce(log *eventlog.Log) bool {
	return b.emitSnapshot(log, true)
}

func (b *Board) emitSnapshot(log *eventlog.Log, force bool) bool {
	if log == nil {
		return false
	}
	b.mu.Lock()
	now := time.Now()
	if !force && b.minInterval > 0 && !b.lastEmit.IsZero() && now.Sub(b.lastEmit) < b.minInterval {
		b.mu.Unlock()
		return false // coalesced: too soon since the last emit
	}
	b.lastEmit = now
	snap := b.snapshotLocked()
	b.mu.Unlock()

	// Append OUTSIDE the Board lock: the log has its own mutex, and holding both would
	// serialize the whole swarm behind a disk write. The snapshot was taken atomically
	// above, so the Detail is a consistent point-in-time tally.
	log.Append(eventlog.Event{
		Kind: ScoreboardSnapshotKind,
		Detail: map[string]any{
			detailPass:      snap.Pass,
			detailChecked:   snap.Checked,
			detailPassed:    snap.Passed,
			detailFailed:    snap.Failed,
			detailRetryPass: snap.RetryPass,
			detailRemaining: snap.Remaining,
		},
	})
	return true
}
