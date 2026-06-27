package experience

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/store"
)

// Projector is the SINGLE write path of the experience layer: it folds the
// append-only event log forward into the store's derived projection tables
// (EXP-T03). The projection is never authoritative — Rebuild re-derives it purely
// from the log, so it is droppable and rebuildable (I5) — and it folds ONLY
// verifier-judged race_outcome verdicts, never a backend self-report (I2). Many
// readers (the router, planner, auto-approval, OverStore) read the projection;
// this is the only writer.
type Projector struct {
	s *store.Store
}

// NewProjector returns a projector that writes into s's projection tables.
func NewProjector(s *store.Store) *Projector { return &Projector{s: s} }

// acc is a per-backend running accumulation while folding the log.
type acc struct {
	races, passes int64
	cost          float64
	latency       int64
	lastSeen      time.Time
}

// projEvent decodes only the fields the projection folds (chain integrity is
// eventlog.Verify's job, so seq/prev/hash beyond Seq are ignored).
type projEvent struct {
	Seq     uint64         `json:"seq"`
	Time    time.Time      `json:"time"`
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// Rebuild drops nothing it shouldn't and re-derives the whole projection from the
// append-only log: it replays every race_outcome into per-backend standings,
// records the watermark + chain status in exp_meta, and FAILS CLOSED on a broken
// chain (it records chain_ok=0 and returns the verify error, so no reader ranks
// over a tampered log — I5). The log being append-only, re-running Rebuild
// upserts the same rows, so it is idempotent for a given log.
func (p *Projector) Rebuild(ctx context.Context, logPath string) error {
	stands := map[string]*acc{}
	var lastSeq int64

	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No history ⇒ an empty, vacuously-verified projection.
			return p.s.SetExpMeta(ctx, store.ExpMeta{SourceSeq: 0, ChainOK: true, RebuiltAt: time.Now().UTC()})
		}
		return fmt.Errorf("opening event log: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e projEvent
		if err := json.Unmarshal(line, &e); err != nil {
			f.Close()
			return fmt.Errorf("event %d: parsing line: %w", n, err)
		}
		if int64(e.Seq) > lastSeq {
			lastSeq = int64(e.Seq)
		}
		applyRaceOutcome(stands, e)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return fmt.Errorf("reading event log: %w", err)
	}
	f.Close()

	verr := eventlog.Verify(logPath)
	for backend, a := range stands {
		if err := p.s.UpsertBackendStanding(ctx, store.BackendStanding{
			Class: "", Backend: backend, Races: a.races, Passes: a.passes,
			CostUSD: a.cost, LatencyNS: a.latency, LastSeen: a.lastSeen,
		}); err != nil {
			return fmt.Errorf("writing standing: %w", err)
		}
	}
	if err := p.s.SetExpMeta(ctx, store.ExpMeta{SourceSeq: lastSeq, ChainOK: verr == nil, RebuiltAt: time.Now().UTC()}); err != nil {
		return err
	}
	if verr != nil {
		return fmt.Errorf("verifying chain: %w", verr)
	}
	return nil
}

// Fold incrementally folds ONE already-durable event into the projection. It is
// idempotent via the exp_meta.source_seq watermark (folding an event at or below
// the watermark is a no-op), so a replayed or duplicated event never double-counts.
// Only verifier-judged race_outcome events change state (I2).
func (p *Projector) Fold(ctx context.Context, e eventlog.Event) error {
	if e.Kind != "race_outcome" {
		return nil
	}
	meta, ok, err := p.s.ExpMeta(ctx)
	if err != nil {
		return err
	}
	// A fresh projection (no meta row yet) has folded NOTHING — not even seq 0. Only
	// once a meta row exists is SourceSeq a real high-water mark, so an event at or
	// below it is already folded (idempotent). Distinguishing the two lets a
	// race_outcome that is the literal first log event (seq 0) fold under live
	// activation instead of being dropped by a spurious 0 <= 0 test.
	if ok && int64(e.Seq) <= meta.SourceSeq {
		return nil // already folded (idempotent)
	}

	cur, err := p.s.BackendStandings(ctx, "")
	if err != nil {
		return err
	}
	a := &acc{}
	for _, bs := range cur {
		if bs.Backend == e.Backend {
			a = &acc{races: bs.Races, passes: bs.Passes, cost: bs.CostUSD, latency: bs.LatencyNS, lastSeen: bs.LastSeen}
			break
		}
	}
	applyRaceOutcome(map[string]*acc{e.Backend: a}, projEvent{Kind: e.Kind, Backend: e.Backend, Detail: e.Detail, Time: e.Time})
	if err := p.s.UpsertBackendStanding(ctx, store.BackendStanding{
		Class: "", Backend: e.Backend, Races: a.races, Passes: a.passes,
		CostUSD: a.cost, LatencyNS: a.latency, LastSeen: a.lastSeen,
	}); err != nil {
		return err
	}
	meta.SourceSeq = int64(e.Seq)
	return p.s.SetExpMeta(ctx, meta)
}

// applyRaceOutcome folds one event into the per-backend accumulator. It is the
// ONE place an event becomes projection state, so Rebuild and Fold agree by
// construction. A self-claim with no verifier verdict never folds to a pass (I2).
func applyRaceOutcome(stands map[string]*acc, e projEvent) {
	if e.Kind != "race_outcome" {
		return
	}
	a := stands[e.Backend]
	if a == nil {
		a = &acc{}
		stands[e.Backend] = a
	}
	a.races++
	if passed, _ := e.Detail["passed"].(bool); passed {
		a.passes++
	}
	if c, ok := floatOf(e.Detail["cost"]); ok {
		a.cost += c
	}
	if v, ok := floatOf(e.Detail["latency_ns"]); ok {
		a.latency += int64(v)
	}
	if e.Time.After(a.lastSeen) {
		a.lastSeen = e.Time
	}
}
