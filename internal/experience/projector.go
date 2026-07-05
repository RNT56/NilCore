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
// verifier-judged evidence: race_outcome verdicts into per-(class, backend)
// standings and selfeval_report records into per-config standings (selfeval.Fold
// emits those ONLY over a verified chain and a verifier-judged report — I2/I5),
// never a backend self-report (I2). Many readers (the router, planner,
// auto-approval, OverStore) read the projection; this is the only writer.
type Projector struct {
	s *store.Store
}

// NewProjector returns a projector that writes into s's projection tables.
func NewProjector(s *store.Store) *Projector { return &Projector{s: s} }

// acc is a per-(class, backend) running accumulation while folding the log.
type acc struct {
	races, passes int64
	cost          float64
	latency       int64
	lastSeen      time.Time
}

// standKey keys the backend accumulator on (class, backend). The empty class ("")
// is the GLOBAL bucket every race also folds into (so a class-less `-class ""`
// query reads the whole scoreboard), mirroring trust.Ledger's classKey where ""
// is "the cell view of the global per-backend ledger".
type standKey struct {
	class   string
	backend string
}

// confAcc is a per-config eval rollup while folding selfeval_report events. Eval
// reports are SNAPSHOTS (the latest measurement wins), not increments, so folding
// overwrites rather than accumulates — mirroring trust.Ledger.FoldEvalReport.
type confAcc struct {
	passRate  float64
	totalCost float64
	cases     int64
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
// append-only log: it replays every race_outcome into per-(class, backend)
// standings AND every selfeval_report into per-config standings, records the
// watermark + chain status in exp_meta, and FAILS CLOSED on a broken chain (it
// records chain_ok=0 and returns the verify error, so no reader ranks over a
// tampered log — I5). The log being append-only, re-running Rebuild upserts the
// same rows, so it is idempotent for a given log.
func (p *Projector) Rebuild(ctx context.Context, logPath string) error {
	stands := map[standKey]*acc{}
	confs := map[string]*confAcc{}
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
		applySelfevalReport(confs, e)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return fmt.Errorf("reading event log: %w", err)
	}
	f.Close()

	verr := eventlog.Verify(logPath)
	for k, a := range stands {
		if err := p.s.UpsertBackendStanding(ctx, store.BackendStanding{
			Class: k.class, Backend: k.backend, Races: a.races, Passes: a.passes,
			CostUSD: a.cost, LatencyNS: a.latency, LastSeen: a.lastSeen,
		}); err != nil {
			return fmt.Errorf("writing standing: %w", err)
		}
	}
	for config, c := range confs {
		if err := p.s.UpsertConfigStanding(ctx, store.ConfigStanding{
			Config: config, PassRate: c.passRate, TotalCost: c.totalCost, Cases: c.cases,
		}); err != nil {
			return fmt.Errorf("writing config standing: %w", err)
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
// Only verifier-judged events change state (I2): race_outcome verdicts fold into
// per-(class, backend) standings, and selfeval_report records (emitted by
// selfeval.Fold only over a verified chain) fold into per-config standings. Every
// other event kind is a no-op.
func (p *Projector) Fold(ctx context.Context, e eventlog.Event) error {
	if e.Kind != "race_outcome" && e.Kind != "selfeval_report" {
		return nil
	}
	meta, ok, err := p.s.ExpMeta(ctx)
	if err != nil {
		return err
	}
	// A fresh projection (no meta row yet) has folded NOTHING — not even seq 0. Only
	// once a meta row exists is SourceSeq a real high-water mark, so an event at or
	// below it is already folded (idempotent). Distinguishing the two lets an event
	// that is the literal first log entry (seq 0) fold under live activation instead
	// of being dropped by a spurious 0 <= 0 test.
	if ok && int64(e.Seq) <= meta.SourceSeq {
		return nil // already folded (idempotent)
	}

	pe := projEvent{Kind: e.Kind, Backend: e.Backend, Detail: e.Detail, Time: e.Time}
	switch e.Kind {
	case "race_outcome":
		if err := p.foldRace(ctx, pe); err != nil {
			return err
		}
	case "selfeval_report":
		if err := p.foldSelfeval(ctx, pe); err != nil {
			return err
		}
	}
	meta.SourceSeq = int64(e.Seq)
	return p.s.SetExpMeta(ctx, meta)
}

// foldRace incrementally folds one race_outcome into the per-(class, backend)
// standings. It updates BOTH the event's real class bucket and the global "" bucket
// (so a class-less query still sees the whole scoreboard), reading each row's
// current running totals first so the fold is additive.
func (p *Projector) foldRace(ctx context.Context, e projEvent) error {
	class, _ := e.Detail["class"].(string)
	classes := []string{""}
	if class != "" {
		classes = append(classes, class)
	}
	for _, cl := range classes {
		a, err := p.loadStanding(ctx, cl, e.Backend)
		if err != nil {
			return err
		}
		stands := map[standKey]*acc{{class: cl, backend: e.Backend}: a}
		// Fold into a single bucket: rewrite Detail["class"] to the target so
		// applyRaceOutcome keys it there (avoids double-folding the global bucket).
		ev := e
		if e.Detail != nil {
			d := make(map[string]any, len(e.Detail))
			for k, v := range e.Detail {
				d[k] = v
			}
			d["class"] = cl
			ev.Detail = d
		}
		applyRaceOutcome(stands, ev)
		if err := p.s.UpsertBackendStanding(ctx, store.BackendStanding{
			Class: cl, Backend: e.Backend, Races: a.races, Passes: a.passes,
			CostUSD: a.cost, LatencyNS: a.latency, LastSeen: a.lastSeen,
		}); err != nil {
			return err
		}
	}
	return nil
}

// loadStanding reads the current running totals for (class, backend), or a zero
// accumulator when no row exists yet.
func (p *Projector) loadStanding(ctx context.Context, class, backend string) (*acc, error) {
	cur, err := p.s.BackendStandings(ctx, class)
	if err != nil {
		return nil, err
	}
	for _, bs := range cur {
		if bs.Backend == backend {
			return &acc{races: bs.Races, passes: bs.Passes, cost: bs.CostUSD, latency: bs.LatencyNS, lastSeen: bs.LastSeen}, nil
		}
	}
	return &acc{}, nil
}

// foldSelfeval incrementally folds one selfeval_report into exp_config_standing.
// Eval reports are snapshots (latest wins), so this OVERWRITES the config's row.
func (p *Projector) foldSelfeval(ctx context.Context, e projEvent) error {
	confs := map[string]*confAcc{}
	applySelfevalReport(confs, e)
	for config, c := range confs {
		if err := p.s.UpsertConfigStanding(ctx, store.ConfigStanding{
			Config: config, PassRate: c.passRate, TotalCost: c.totalCost, Cases: c.cases,
		}); err != nil {
			return err
		}
	}
	return nil
}

// applyRaceOutcome folds one race_outcome into the per-(class, backend)
// accumulator. It is the ONE place a race becomes projection state, so Rebuild and
// Fold agree by construction. A self-claim with no verifier verdict never folds to
// a pass (I2). Each event is folded into BOTH its declared class (Detail["class"],
// default "") AND the global "" bucket, so a class-less query reads the whole
// scoreboard while a `-class X` query reads only that class's races — keeping the
// warm (store) and log-replay paths consistent.
func applyRaceOutcome(stands map[standKey]*acc, e projEvent) {
	if e.Kind != "race_outcome" {
		return
	}
	class, _ := e.Detail["class"].(string)
	keys := []standKey{{class: "", backend: e.Backend}}
	if class != "" {
		keys = append(keys, standKey{class: class, backend: e.Backend})
	}
	for _, k := range keys {
		a := stands[k]
		if a == nil {
			a = &acc{}
			stands[k] = a
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
}

// applySelfevalReport folds one selfeval_report (emitted by flywheel selfeval.Fold
// ONLY over a verified chain and a verifier-judged report — I2/I5) into the
// per-config eval rollup. Eval reports are SNAPSHOTS: re-folding a config by the
// same name OVERWRITES it (the latest measurement wins, never double-counts),
// mirroring trust.Ledger.FoldEvalReport. A report with no config key carries
// nothing to attribute and is skipped. The selfeval_report detail carries no cost,
// so TotalCost stays 0 (exactly as trust.Replay folds these). The wire literal is
// used (not selfeval.EventKind) to keep experience from importing the flywheel.
func applySelfevalReport(confs map[string]*confAcc, e projEvent) {
	if e.Kind != "selfeval_report" {
		return
	}
	config, _ := e.Detail["config"].(string)
	if config == "" {
		return
	}
	rate, _ := floatOf(e.Detail["pass_rate"])
	cases, _ := floatOf(e.Detail["cases"])
	cost, _ := floatOf(e.Detail["cost"]) // absent today ⇒ 0
	confs[config] = &confAcc{passRate: rate, totalCost: cost, cases: int64(cases)}
}
