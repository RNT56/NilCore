package experience

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/trust"
)

// logEvent mirrors only the fields of an on-disk eventlog.Event that carry
// verifier signal. Chain integrity is eventlog.Verify's job, not this decoder's,
// so the seq/prev/hash fields are intentionally ignored here.
type logEvent struct {
	Time    time.Time      `json:"time"`
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// clsAgg accumulates a per-class outcome rollup while replaying the log (the
// samples are held per-class so the median is over that class's contests only).
type clsAgg struct {
	agg         Aggregate
	costs, lats []float64
}

// OverLog builds a read-only Experience by replaying the append-only event log
// at logPath, then verifying its hash chain. It folds the verifier-judged
// race_outcome events (Backend + Detail["passed"]) into the trust scoreboard and
// the outcome Aggregate (counts + cost/latency samples + last-seen), and runs
// eventlog.Verify LAST: a broken chain returns the verifier's error and a nil
// reader, so no standing is ever earned from a tampered log (fail-closed, I5),
// exactly like trust.Replay.
//
// A MISSING log is a clean empty reader (nil error): a fresh install has no
// earned signal yet, which every consumer reads as "no evidence" (and an
// auto-approval bar therefore fails). Only an EXISTING but unreadable/broken log
// errors. No backend self-report ever folds to a pass (I2): only Detail["passed"]
// counts, and an absent verdict reads as a non-pass.
func OverLog(logPath string) (*Experience, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No history ⇒ clean empty reader. An empty log is vacuously verified.
			return &Experience{chainOK: true}, nil
		}
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	// One ledger folds every race into its (class, backend) cell AND the global ""
	// cell (trust.Record keys "" as the global-view cell), so a class-less query
	// reads the whole scoreboard while a `-class X` query reads that class only —
	// the same split the store-backed projection makes, keeping the two paths
	// consistent. Per-class outcome rollups accumulate alongside so Outcomes filters
	// by class too.
	l := trust.New()
	byCls := map[string]*clsAgg{"": {}} // "" = the global rollup
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e logEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("event %d: parsing line: %w", n, err)
		}
		if e.Kind != "race_outcome" {
			continue // only verifier-judged race outcomes carry standing signal
		}
		class, _ := e.Detail["class"].(string)
		passed, _ := e.Detail["passed"].(bool)
		// One Record per race: it folds the GLOBAL backend map (which backs
		// BackendStanding("")) once AND the class cell (which backs a `-class X`
		// query). No second fold — that would double-count the global scoreboard.
		l.Record(trust.Outcome{Backend: e.Backend, Class: class, Passed: passed})

		clss := []string{""}
		if class != "" {
			clss = append(clss, class)
		}
		for _, cl := range clss {
			ca := byCls[cl]
			if ca == nil {
				ca = &clsAgg{}
				byCls[cl] = ca
			}
			ca.agg.Races++
			if passed {
				ca.agg.Passes++
			}
			if c, ok := floatOf(e.Detail["cost"]); ok {
				ca.costs = append(ca.costs, c)
			}
			if v, ok := floatOf(e.Detail["latency_ns"]); ok {
				ca.lats = append(ca.lats, v)
			}
			if e.Time.After(ca.agg.LastSeen) {
				ca.agg.LastSeen = e.Time
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading event log: %w", err)
	}

	// Chain integrity is eventlog's authority: drop everything we just folded if
	// the chain does not link (no ranking over forged evidence, I5).
	if err := eventlog.Verify(logPath); err != nil {
		return nil, fmt.Errorf("verifying chain: %w", err)
	}

	aggByCls := make(map[string]Aggregate, len(byCls))
	for cl, ca := range byCls {
		ca.agg.MedianCostUSD = median(ca.costs)
		ca.agg.MedianLatency = median(ca.lats)
		ca.agg.Class = cl
		aggByCls[cl] = ca.agg
	}
	snap := l.Snapshot()
	return &Experience{
		backends: snap.Backends,
		byClass:  classStandings(snap),
		configs:  snap.Configs,
		agg:      aggByCls[""],
		aggByCls: aggByCls,
		chainOK:  true,
	}, nil
}

// classStandings groups a trust snapshot's per-class cells into per-class Stat
// slices (best-first within a class, as the snapshot already orders them). The
// global "" cells are omitted — a class-less query reads the global Backends
// scoreboard directly (which is byte-identical to the "" cells).
func classStandings(snap trust.Snapshot) map[string][]trust.Stat {
	out := map[string][]trust.Stat{}
	for _, c := range snap.Classes {
		if c.Class == "" {
			continue // the global bucket is served by Backends
		}
		out[c.Class] = append(out[c.Class], trust.Stat{
			Backend:  c.Backend,
			Races:    c.Races,
			Wins:     c.Wins,
			PassRate: c.PassRate,
		})
	}
	return out
}
