// Package inspect gives an operator read-only observability over the append-only
// event log (P6-T07). The eventlog package is the writer and the authority on
// chain integrity; inspect never mutates the log, it only replays it to answer
// two operational questions: "what happened?" (a Summary of events by kind and
// the distinct tasks seen) and "is the system healthy?" (a readiness check that
// the log is both readable and tamper-free). Integrity is delegated to
// eventlog.Verify so there is one source of truth for the hash chain.
package inspect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"nilcore/internal/eventlog"
)

// Summary is the operator-facing rollup of one event log: the total number of
// events, a count per event kind, and the distinct task ids observed (in first-
// seen order, so the report is stable and reproducible).
type Summary struct {
	Total  int
	ByKind map[string]int
	Tasks  []string
}

// event mirrors the on-disk JSONL shape written by eventlog. inspect reads only
// the fields it reports on; unknown fields are ignored by encoding/json.
type event struct {
	Task string `json:"task"`
	Kind string `json:"kind"`
}

// Replay parses every line of the log at path, counting events by kind and
// collecting the distinct task ids, then verifies the hash chain via
// eventlog.Verify. If the chain is broken it returns Verify's error and an empty
// Summary, so a corrupt log can never be reported as if it were trustworthy.
func Replay(path string) (Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Summary{}, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	s := Summary{ByKind: map[string]int{}}
	seen := map[string]bool{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			return Summary{}, fmt.Errorf("event %d: parsing line: %w", n, err)
		}
		s.Total++
		s.ByKind[e.Kind]++
		if e.Task != "" && !seen[e.Task] {
			seen[e.Task] = true
			s.Tasks = append(s.Tasks, e.Task)
		}
	}
	if err := sc.Err(); err != nil {
		return Summary{}, fmt.Errorf("reading event log: %w", err)
	}

	// Chain integrity is the eventlog package's call, not ours: a parseable log
	// whose hashes do not link is still untrustworthy and must surface as error.
	if err := eventlog.Verify(path); err != nil {
		return Summary{}, fmt.Errorf("verifying chain: %w", err)
	}
	return s, nil
}

// Health is a readiness probe: it returns nil only when the log at path is
// readable and its hash chain verifies. Any read error or chain break is
// returned wrapped, so an operator (or a liveness endpoint) gets a single
// pass/fail signal for the audit trail.
func Health(path string) error {
	if _, err := Replay(path); err != nil {
		return fmt.Errorf("event log unhealthy: %w", err)
	}
	return nil
}
