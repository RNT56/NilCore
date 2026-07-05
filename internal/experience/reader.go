// Package experience is the unified read layer over NilCore's learned state
// (Phase 16, docs/ROADMAP-CLOSED-LOOP.md Pillar 1). It folds four otherwise-
// fragmented evidence sources — the verifier-judged trust scoreboard (replayed
// race_outcome events), per-config eval rollups, cross-project memory, and the
// replayed event-log outcome tally — behind ONE Reader interface, so the
// router, planner, self-improvement loop, and graduated-auto-approval policy all
// consult one surface instead of each re-deriving its own.
//
// Two invariants shape the design:
//
//   - I5: the append-only event log stays the SOLE source of truth. This package
//     never mutates it; it only REPLAYS it read-only (OverLog), or reads a
//     derived, rebuildable projection (OverStore, a later task). Every reader
//     fails closed on a broken hash chain — a log we cannot trust yields no
//     ranking, exactly like trust.Replay (internal/trust/replay.go:78).
//   - I2: the projection is built ONLY from verifier verdicts (race_outcome's
//     Detail["passed"], verify-family events). No backend self-report ever folds
//     to a pass, and no reader can mark work "done", auto-approve a gate, or skip
//     the verifier — the Reader returns standings, outcomes, and lessons, which
//     are inputs to ORDERING and CONTEXT, never to the ship/no-ship decision.
//
// The package is a leaf: it imports trust / memory / eventlog (+ their stdlib
// closure) and never the orchestrator (deps_test.go enforces this). A nil Reader
// degrades every consumer to today's static behaviour, so the loop is byte-
// identical when experience is not wired.
package experience

import (
	"context"

	"nilcore/internal/memory"
	"nilcore/internal/trust"
)

// Reader is the single query surface over learned state. Every method is
// ctx-first and returns typed value snapshots (never a live handle). A reader is
// fail-closed: construction returns an error over a broken hash chain, so a
// successfully-built reader is always over a verified (or empty) log.
type Reader interface {
	// BackendStanding returns the verifier-judged per-backend scoreboard. The
	// taskClass argument is reserved for the per-class cell dimension that
	// Pillar 2 (RTE) adds; today the standing is global and taskClass is echoed
	// for forward-compatibility, not yet used to filter.
	BackendStanding(ctx context.Context, taskClass string) ([]trust.Stat, error)
	// ConfigStanding returns the per-config eval rollup (pass-rate / cost / cases).
	// Eval reports are a separate input folded by the store-backed reader; the
	// log-only reader reports an empty rollup.
	ConfigStanding(ctx context.Context) ([]trust.ConfigStat, error)
	// Lessons returns memory records (the "data, not instructions" corpus, I7)
	// matching scope/project/keyword, capped at max (<=0 = uncapped). The log-only
	// reader has no memory backend and returns none.
	Lessons(ctx context.Context, scope, project, keyword string, max int) ([]memory.Record, error)
	// Outcomes returns the verifier-judged outcome rollup (races / passes / median
	// cost+latency / last-seen). taskClass is echoed (see BackendStanding).
	Outcomes(ctx context.Context, taskClass string) (Aggregate, error)
	// ChainVerified reports whether the underlying log's hash chain verified at
	// read time. A fail-closed consumer (e.g. graduated auto-approval) must treat
	// false as "no earned trust".
	ChainVerified(ctx context.Context) (bool, error)
}

// Experience is the concrete Reader. Build it with OverLog (replay the JSONL log
// directly) or OverStore (read the derived projection; a later task). The zero
// value is a valid empty reader (no history ⇒ no earned signal).
type Experience struct {
	backends []trust.Stat            // the GLOBAL (class "") scoreboard
	byClass  map[string][]trust.Stat // per-class scoreboards ("" ⇒ global); nil ⇒ class filter degrades to global
	configs  []trust.ConfigStat
	agg      Aggregate            // the GLOBAL (class "") outcome rollup
	aggByCls map[string]Aggregate // per-class outcome rollups ("" ⇒ global); nil ⇒ class filter degrades to global
	chainOK  bool
	mem      *memory.Memory // nil for the log-only reader (memory lives in the store)
}

// BackendStanding returns the verifier-judged scoreboard for taskClass. An empty
// taskClass ("") reads the GLOBAL scoreboard (every race, class-agnostic); a
// non-empty class reads only that class's races. This mirrors the store-backed
// reader (OverStore queries exp_backend_standing by the same class column), so the
// warm and log-replay paths agree on `-class` filtering. An unknown class yields no
// standings (no evidence for that bucket), never the global scoreboard.
func (x *Experience) BackendStanding(_ context.Context, taskClass string) ([]trust.Stat, error) {
	if x == nil {
		return nil, nil
	}
	if taskClass == "" {
		return x.backends, nil
	}
	if x.byClass == nil {
		return nil, nil
	}
	return x.byClass[taskClass], nil
}

// ConfigStanding returns the per-config eval rollup.
func (x *Experience) ConfigStanding(_ context.Context) ([]trust.ConfigStat, error) {
	if x == nil {
		return nil, nil
	}
	return x.configs, nil
}

// Lessons reads memory through internal/memory (data, not instructions — I7).
// With no memory backend wired (the log-only reader) it returns none.
func (x *Experience) Lessons(ctx context.Context, scope, project, keyword string, max int) ([]memory.Record, error) {
	if x == nil || x.mem == nil {
		return nil, nil
	}
	recs, err := x.mem.Query(ctx, scope, project, keyword)
	if err != nil {
		return nil, err
	}
	if max > 0 && len(recs) > max {
		recs = recs[:max]
	}
	return recs, nil
}

// Outcomes returns the verifier-judged outcome rollup for taskClass. An empty
// taskClass ("") is the GLOBAL rollup (every race); a non-empty class is that
// class's rollup only — consistent with BackendStanding and the store-backed
// reader (which sums exp_backend_standing rows for the same class). An unknown
// class yields a zero rollup (no evidence for that bucket).
func (x *Experience) Outcomes(_ context.Context, taskClass string) (Aggregate, error) {
	if x == nil {
		return Aggregate{Class: taskClass}, nil
	}
	if taskClass == "" {
		a := x.agg
		a.Class = ""
		return a, nil
	}
	a := x.aggByCls[taskClass] // zero Aggregate for an unknown class
	a.Class = taskClass
	return a, nil
}

// ChainVerified reports the last replay's chain status. A successfully-built
// OverLog reader is always true (a broken chain returns an error from OverLog
// instead of a reader), so a false here only ever comes from a store projection
// that recorded a broken-chain fold.
func (x *Experience) ChainVerified(_ context.Context) (bool, error) {
	if x == nil {
		return false, nil
	}
	return x.chainOK, nil
}
