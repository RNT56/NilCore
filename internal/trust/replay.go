package trust

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"nilcore/internal/eventlog"
)

// raceEvent mirrors only the fields of an on-disk eventlog.Event that a
// race_outcome carries trust signal in: the Backend that raced, the verifier's
// pass/fail verdict (Detail["passed"]), and — added in Phase 16 — the task-class
// bucket (Detail["class"]) and verifier-judged cost (Detail["cost"]) the router
// writes. Every other field (time, seq, hash, …) is ignored by encoding/json —
// the chain integrity is eventlog.Verify's job, not ours.
type raceEvent struct {
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// Replay builds a Ledger by replaying the append-only event log at logPath
// READ-ONLY: it scans every JSONL line, folds each `race_outcome` event (Backend
// + the verifier's Detail["passed"] verdict) into the per-backend scoreboard,
// then — and only then — runs eventlog.Verify on the same file. If the hash chain
// is broken (tampered, reordered, dropped, or corrupt) it returns Verify's error
// and a nil ledger: a log we cannot trust yields NO trustworthy ranking, so we
// fail closed exactly as inspect.Replay does. This is the trust angle — strength
// routing must never be earned from forged evidence.
//
// A MISSING log is a clean empty ledger (nil error), not a failure: a fresh
// install with no history simply has no earned signal yet, which the Router reads
// as "use the default backend". Only an EXISTING but unreadable/broken log errors.
func Replay(logPath string) (*Ledger, error) {
	f, err := os.Open(logPath)
	if err != nil {
		// No log yet ⇒ no history ⇒ a clean empty ledger. Any other open error
		// (permissions, a directory, an I/O fault) is a real failure to surface.
		if errors.Is(err, fs.ErrNotExist) {
			return New(), nil
		}
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	l := New()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e raceEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("event %d: parsing line: %w", n, err)
		}
		if e.Kind != "race_outcome" {
			continue // only verifier-judged race outcomes carry routing signal
		}
		// Detail["passed"] is the verifier's verdict, written as a JSON bool by
		// route.Race. A missing or non-bool value reads as a non-pass (fail-safe:
		// absent evidence never counts as a win).
		passed, _ := e.Detail["passed"].(bool)
		// Detail["class"] / Detail["cost"] are the Phase-16 routing dimensions.
		// Both are OPTIONAL: a pre-Phase-16 race_outcome carries neither, so class
		// reads as "" (folding into the global-view cell exactly as before) and
		// cost reads as 0. JSON decodes numbers as float64, so a present cost lands
		// directly. A missing or wrong-typed value is the zero value (fail-safe).
		class, _ := e.Detail["class"].(string)
		cost, _ := e.Detail["cost"].(float64)
		l.Record(Outcome{Backend: e.Backend, Class: class, Passed: passed, Cost: cost})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading event log: %w", err)
	}

	// Chain integrity is eventlog's authority, not ours: a parseable log whose
	// hashes do not link is untrustworthy and must surface as an error AFTER we
	// drop the ledger we just built from it (fail-closed — no ranking over a
	// tampered chain).
	if err := eventlog.Verify(logPath); err != nil {
		return nil, fmt.Errorf("verifying chain: %w", err)
	}
	return l, nil
}
