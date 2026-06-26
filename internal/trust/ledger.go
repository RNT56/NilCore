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
//
// Class is the deterministic task-class bucket the attempt belonged to (see
// classify.go), keying the per-(class, backend) cell. It is OPTIONAL and
// defaults to the empty string: an Outcome with Class == "" folds into the
// global per-backend scoreboard EXACTLY as before and additionally into the
// "" class cell, so a caller that never sets Class observes today's behaviour
// byte-identically. Class is a routing hint only (I2) — it never judges work.
type Outcome struct {
	Backend string
	Config  string
	Class   string
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

// ClassStat is the per-(task-class, backend) cell: how a single backend has done
// on a single task class. It carries the same race/win rollup as Stat plus the
// accumulated verifier-judged cost for that cell, so cost-aware routing (RTE-T06)
// can read "what has this backend cost me on REFACTOR tasks, and how often did it
// pass". PassRate is the raw observed rate (display-only); ranking still uses the
// smoothed score (see classScore). Cost accumulates additively per attempt.
type ClassStat struct {
	Class     string
	Backend   string
	Races     int
	Wins      int
	PassRate  float64 // raw Wins/Races (0 when Races == 0); display-only
	TotalCost float64
}

// classKey keys the per-class cell on the (class, backend) pair. A zero-value
// Class ("") is a first-class key, not a sentinel — it is the bucket every
// Class-less Outcome folds into, so the "" class is simply "the cell view of the
// global per-backend ledger".
type classKey struct {
	class   string
	backend string
}

// Ledger accumulates verifier-judged evidence: per-backend race outcomes, the
// per-(class, backend) cells, and per-config eval reports. It is an in-memory
// fold — the durable record is the event log, which Replay reads. A Ledger is not
// safe for concurrent mutation; build it (Record / FoldEvalReport / Replay) then
// Snapshot for read-out.
type Ledger struct {
	backends map[string]*Stat
	classes  map[classKey]*ClassStat
	configs  map[string]ConfigStat
}

// New returns an empty ledger ready to Record into.
func New() *Ledger {
	return &Ledger{
		backends: map[string]*Stat{},
		classes:  map[classKey]*ClassStat{},
		configs:  map[string]ConfigStat{},
	}
}

// Record folds one verifier-judged outcome into the per-backend scoreboard AND
// into the per-(class, backend) cell keyed by o.Class (default "" — see Outcome).
// An empty backend name is ignored (a race_outcome with no backend carries no
// attributable signal). The global per-backend fold is unchanged; the class cell
// is an additive second view that also accumulates o.Cost, so a caller that never
// sets Class still sees today's global scoreboard exactly. The config dimension is
// folded separately, via FoldEvalReport, because a single eval report aggregates
// many cases.
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

	// Per-(class, backend) cell. o.Class == "" is the global-view cell.
	k := classKey{class: o.Class, backend: o.Backend}
	cs := l.classes[k]
	if cs == nil {
		cs = &ClassStat{Class: o.Class, Backend: o.Backend}
		l.classes[k] = cs
	}
	cs.Races++
	if o.Passed {
		cs.Wins++
	}
	cs.PassRate = float64(cs.Wins) / float64(cs.Races)
	cs.TotalCost += o.Cost
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
	Classes  []ClassStat
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

	for _, c := range l.classes {
		snap.Classes = append(snap.Classes, *c)
	}
	sortClassStats(snap.Classes)
	return snap
}

// sortClassStats orders class cells deterministically: by class name first, then
// best-first by smoothed score within a class, ties broken by backend name. So a
// Snapshot's Classes slice is a stable per-class scoreboard.
func sortClassStats(cs []ClassStat) {
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		sa, sb := classScore(a), classScore(b)
		if math.Abs(sa-sb) > 1e-12 {
			return sa > sb
		}
		return a.Backend < b.Backend
	})
}

// score is the SMOOTHED pass-rate used for ranking. We use additive (Laplace /
// "rule of succession") smoothing toward a 0.5 prior with pseudo-count alpha:
//
//	score = (Wins + alpha) / (Races + 2*alpha)
//
// This pulls thin-evidence backends toward 0.5 and lets evidence overcome the
// prior as races accumulate, so a 1-of-1 backend (score ≈ 0.667 at alpha=1) does
// NOT outrank a 90-of-100 one (score ≈ 0.892). We chose Laplace over Wilson for
// its single legible knob and stdlib-only arithmetic; alpha=1 is the classic
// rule-of-succession choice and is strong enough that a single lucky sample never
// wins. A backend with zero races scores exactly the 0.5 prior — unproven, not
// trusted, not distrusted.
const smoothingAlpha = 1.0

func score(s Stat) float64 {
	return (float64(s.Wins) + smoothingAlpha) / (float64(s.Races) + 2*smoothingAlpha)
}

// classScore is the same Laplace-smoothed pass rate as score, applied to a
// per-class cell. Sharing the smoothing keeps per-class ranking consistent with
// the global scoreboard: a thin 1-of-1 cell never leapfrogs a well-proven one.
func classScore(c ClassStat) float64 {
	return (float64(c.Wins) + smoothingAlpha) / (float64(c.Races) + 2*smoothingAlpha)
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

// ClassStandings returns the per-(class, backend) cells for a single task class,
// ordered best-first by the SAME smoothed score the global scoreboard uses (ties
// broken by backend name). Passing "" returns the global-view cells — the cell
// projection of the per-backend ledger. A class with no recorded evidence returns
// nil (a nil slice ranges zero times, so callers need no special case). The
// returned slice is fresh, so the caller cannot mutate the ledger through it.
func (l *Ledger) ClassStandings(class string) []ClassStat {
	var out []ClassStat
	for k, c := range l.classes {
		if k.class == class {
			out = append(out, *c)
		}
	}
	if out == nil {
		return nil
	}
	sortClassStats(out) // already class-grouped (single class), so this orders by score
	return out
}
