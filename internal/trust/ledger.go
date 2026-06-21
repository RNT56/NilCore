// Package trust is the Trust Ledger (Phase 13): it reads back the already-logged
// but never-consumed verifier-judged outcomes — every `race_outcome` event the
// router writes (internal/route.Race) and every measure-first eval report
// (eval.Report) — and folds them into a per-backend / per-config scoreboard that
// EARNS strength-routing. The ledger's only job is to ORDER candidate backends so
// the historically strongest one is tried first or made the default.
//
// It deliberately stays on the right side of invariant I2 (the verifier is the
// only authority on "done"): the ledger BIASES which backend to try first, it
// NEVER decides the race winner or whether work ships. The verifier still judges
// every race (route.Race) and re-runs as the final gate (the orchestrator). Read
// the ledger as "who has earned the first attempt", not "who is right".
//
// Trust also means trusting the evidence: the ledger is built by replaying the
// append-only, hash-chained event log (internal/eventlog) READ-ONLY, and it
// refuses to score over a broken chain (see Replay) — a tampered log yields no
// trustworthy ranking, fail-closed. This is a leaf: it imports only backend,
// eventlog, eval, termui + stdlib, never the orchestrator (enforced by
// deps_test.go).
package trust

import (
	"math"
	"sort"

	"nilcore/eval"
)

// Outcome is the unit folded into the ledger: one backend's verifier-judged
// result on one task attempt. Passed is the verifier's verdict (never a backend
// self-report). Config and Cost are optional context carried from eval reports;
// race_outcome events fold in with just Backend + Passed.
type Outcome struct {
	Backend string
	Config  string
	Passed  bool
	Cost    float64
}

// Stat is the per-backend rollup: how many verifier-judged races this backend
// ran, how many it won (passed), and the raw observed pass rate. The raw rate is
// for display; ranking uses a SMOOTHED rate (see Rank) so a 1-of-1 backend cannot
// leapfrog a 90-of-100 one on a single lucky sample.
type Stat struct {
	Backend  string
	Races    int
	Wins     int
	PassRate float64 // raw Wins/Races (0 when Races == 0); display-only
}

// ConfigStat is the per-config rollup folded from an eval.Report: the config's
// measured pass rate, total cost, and the number of cases it was scored over.
// Configs are evidence the operator reads alongside the backend scoreboard; they
// are not part of the backend ranking (a config can name a model+backend pair).
type ConfigStat struct {
	Config    string
	PassRate  float64
	TotalCost float64
	Cases     int
}

// Ledger accumulates verifier-judged evidence: per-backend race outcomes and
// per-config eval reports. It is an in-memory fold — the durable record is the
// event log, which Replay reads. A Ledger is not safe for concurrent mutation;
// build it (Record / FoldEvalReport / Replay) then Snapshot for read-out.
type Ledger struct {
	backends map[string]*Stat
	configs  map[string]ConfigStat
}

// New returns an empty ledger ready to Record into.
func New() *Ledger {
	return &Ledger{
		backends: map[string]*Stat{},
		configs:  map[string]ConfigStat{},
	}
}

// Record folds one verifier-judged outcome into the per-backend scoreboard. An
// empty backend name is ignored (a race_outcome with no backend carries no
// attributable signal). The config dimension is folded separately, via
// FoldEvalReport, because a single eval report aggregates many cases.
func (l *Ledger) Record(o Outcome) {
	if o.Backend == "" {
		return
	}
	s := l.backends[o.Backend]
	if s == nil {
		s = &Stat{Backend: o.Backend}
		l.backends[o.Backend] = s
	}
	s.Races++
	if o.Passed {
		s.Wins++
	}
	s.PassRate = float64(s.Wins) / float64(s.Races)
}

// FoldEvalReport folds one measure-first eval report (eval.Report) into the
// per-config scoreboard. The report already carries its own verifier-based pass
// rate and total cost; we record those plus the case count so the operator can
// see which config configuration earned its standing and at what cost. A report
// with an empty Config name is ignored (nothing to attribute it to). Re-folding a
// config by the same name OVERWRITES it — eval reports are snapshots, not
// increments, so the latest measurement wins rather than double-counting.
func (l *Ledger) FoldEvalReport(r eval.Report) {
	if r.Config == "" {
		return
	}
	l.configs[r.Config] = ConfigStat{
		Config:    r.Config,
		PassRate:  r.PassRate,
		TotalCost: r.TotalCost,
		Cases:     len(r.Results),
	}
}

// Snapshot is an immutable, deterministically-ordered copy-out of the ledger for
// reading and rendering. Backends are sorted best-first by smoothed score (ties
// broken by name); configs are sorted by name. Holding a Snapshot never sees a
// later mutation of the source Ledger.
type Snapshot struct {
	Backends []Stat
	Configs  []ConfigStat
}

// Snapshot returns an immutable copy of the ledger in deterministic order:
// backends best-first by the SAME smoothed score Rank uses (so the scoreboard and
// the routing order agree), configs by name. The returned slices are fresh, so
// the caller cannot mutate the ledger through them.
func (l *Ledger) Snapshot() Snapshot {
	snap := Snapshot{}
	for _, s := range l.backends {
		snap.Backends = append(snap.Backends, *s)
	}
	sortStatsByScore(snap.Backends)

	for _, c := range l.configs {
		snap.Configs = append(snap.Configs, c)
	}
	sort.Slice(snap.Configs, func(i, j int) bool {
		return snap.Configs[i].Config < snap.Configs[j].Config
	})
	return snap
}

// score is the SMOOTHED pass-rate used for ranking. We use additive (Laplace /
// "rule of succession") smoothing toward a 0.5 prior with pseudo-count alpha:
//
//	score = (Wins + alpha) / (Races + 2*alpha)
//
// This pulls thin-evidence backends toward 0.5 and lets evidence overcome the
// prior as races accumulate, so a 1-of-1 backend (score ≈ 0.625 at alpha=1) does
// NOT outrank a 90-of-100 one (score ≈ 0.892). We chose Laplace over Wilson for
// its single legible knob and stdlib-only arithmetic; alpha=1 is the classic
// rule-of-succession choice and is strong enough that a single lucky sample never
// wins. A backend with zero races scores exactly the 0.5 prior — unproven, not
// trusted, not distrusted.
const smoothingAlpha = 1.0

func score(s Stat) float64 {
	return (float64(s.Wins) + smoothingAlpha) / (float64(s.Races) + 2*smoothingAlpha)
}

// sortStatsByScore orders stats best-first by smoothed score. Ties (including the
// all-zero-race prior) break by backend name, so the order is fully deterministic
// and identical between Snapshot and Rank.
func sortStatsByScore(stats []Stat) {
	sort.Slice(stats, func(i, j int) bool {
		si, sj := score(stats[i]), score(stats[j])
		if math.Abs(si-sj) > 1e-12 {
			return si > sj // higher score first
		}
		return stats[i].Backend < stats[j].Backend // stable tiebreak
	})
}

// Rank returns the known backend names ordered best-first by smoothed pass rate
// (see score). It is the routing order the Router consults: the first name that
// names a wired backend gets the first attempt. Ties break by name for
// determinism. An empty ledger returns nil — "no earned signal", which the Router
// reads as "use the default" (a nil slice ranges zero times, so callers need no
// special case).
func (l *Ledger) Rank() []string {
	if len(l.backends) == 0 {
		return nil
	}
	stats := make([]Stat, 0, len(l.backends))
	for _, s := range l.backends {
		stats = append(stats, *s)
	}
	sortStatsByScore(stats)
	names := make([]string, len(stats))
	for i, s := range stats {
		names[i] = s.Backend
	}
	return names
}

// Order stable-orders an arbitrary input set of backend names best-first by the
// ledger's smoothed score. Names the ledger has NEVER seen sort last (they carry
// no earned evidence — the prior is reserved for backends we have at least
// observed), in their original relative order, so an unknown backend is a
// fallback, never a default. The input slice is not mutated.
func (l *Ledger) Order(backends []string) []string {
	type ranked struct {
		name  string
		known bool
		score float64
		idx   int // original index, for a stable tiebreak among unknowns
	}
	items := make([]ranked, len(backends))
	for i, name := range backends {
		r := ranked{name: name, idx: i}
		if s, ok := l.backends[name]; ok {
			r.known = true
			r.score = score(*s)
		}
		items[i] = r
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.known != b.known {
			return a.known // known backends ahead of unknown ones
		}
		if a.known {
			if math.Abs(a.score-b.score) > 1e-12 {
				return a.score > b.score
			}
			return a.name < b.name // deterministic among knowns
		}
		return a.idx < b.idx // preserve input order among unknowns
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.name
	}
	return out
}
