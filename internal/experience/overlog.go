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

	l := trust.New()
	var (
		agg         Aggregate
		costs, lats []float64
	)
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
		passed, _ := e.Detail["passed"].(bool)
		l.Record(trust.Outcome{Backend: e.Backend, Passed: passed})
		agg.Races++
		if passed {
			agg.Passes++
		}
		if c, ok := floatOf(e.Detail["cost"]); ok {
			costs = append(costs, c)
		}
		if v, ok := floatOf(e.Detail["latency_ns"]); ok {
			lats = append(lats, v)
		}
		if e.Time.After(agg.LastSeen) {
			agg.LastSeen = e.Time
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

	agg.MedianCostUSD = median(costs)
	agg.MedianLatency = median(lats)
	snap := l.Snapshot()
	return &Experience{
		backends: snap.Backends,
		configs:  snap.Configs,
		agg:      agg,
		chainOK:  true,
	}, nil
}
