package graapprove

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"nilcore/internal/eventlog"
)

// ScopeKey identifies a trust bucket: an action class (the GateActionType string)
// paired with the target scope (branch / environment).
type ScopeKey struct {
	Type  string
	Scope string
}

// Tally is the verifier-judged scoreboard for one ScopeKey: how many
// boundary_outcomes passed, the total observed, and when the most recent green
// landed (zero time ⇒ never green).
type Tally struct {
	Green     int
	Total     int
	LastGreen time.Time
}

// TrustView is the read-only projection BuildTrust folds from the event log. It is
// rebuilt per decision so it always reflects the latest durable log, and it carries
// ChainOK so a caller can fail closed on a tampered chain. The tallies map is
// unexported so the view stays read-only.
type TrustView struct {
	tallies map[ScopeKey]Tally
	// ChainOK is false when eventlog.Verify rejected the chain; in that case
	// tallies is EMPTY (a tampered log earns nothing).
	ChainOK bool
}

// Tally reports the scoreboard for a ScopeKey (zero value when absent).
func (v TrustView) Tally(k ScopeKey) Tally {
	if v.tallies == nil {
		return Tally{}
	}
	return v.tallies[k]
}

// boundaryEvent mirrors only the fields a `boundary_outcome` carries trust signal
// in. Every other field (time, seq, hash, …) is ignored by encoding/json — chain
// integrity is eventlog.Verify's job, not ours. We use the event Time as the
// when-green timestamp (it is set by Append and covered by the hash chain).
type boundaryEvent struct {
	Time   time.Time      `json:"time"`
	Kind   string         `json:"kind"`
	Detail map[string]any `json:"detail"`
}

// BuildTrust folds the append-only event log at logPath READ-ONLY into a TrustView.
// It scans every JSONL line, folds each `boundary_outcome` event (keyed by
// Detail["action"] + Detail["scope"], counting Detail["passed"] — the verifier's
// verdict, never a self-report) into per-ScopeKey tallies, then — and only then —
// runs eventlog.Verify on the same file.
//
// Fail-closed semantics (I2, I5):
//   - The numerator counts ONLY boundary_outcome events. auto_approve grants are
//     never counted — no self-reinforcement.
//   - A MISSING log is a clean empty view with ChainOK=true and a nil error (a
//     fresh install simply has no earned signal yet).
//   - A broken/tampered chain returns an EMPTY view with ChainOK=false AND the
//     verify error, so the caller can deny EXPLICITLY (a tampered log can only
//     remove trust, never forge it). A parse / read fault is likewise returned.
func BuildTrust(logPath string) (TrustView, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No log yet ⇒ no history ⇒ a clean empty, trusted-shape view.
			return TrustView{tallies: map[ScopeKey]Tally{}, ChainOK: true}, nil
		}
		return TrustView{tallies: map[ScopeKey]Tally{}}, fmt.Errorf("graapprove: opening event log: %w", err)
	}
	defer f.Close()

	tallies := map[ScopeKey]Tally{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e boundaryEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return TrustView{tallies: map[ScopeKey]Tally{}}, fmt.Errorf("graapprove: event %d: parsing line: %w", n, err)
		}
		if e.Kind != "boundary_outcome" {
			continue // only the dedicated verifier-judged event carries trust signal
		}
		action, _ := e.Detail["action"].(string)
		scope, _ := e.Detail["scope"].(string)
		if action == "" {
			continue // an outcome with no action class carries no routable signal
		}
		// passed is the verifier's verdict, written as a JSON bool. A missing or
		// non-bool value reads as a non-pass (fail-safe: absent evidence is never a
		// win).
		passed, _ := e.Detail["passed"].(bool)

		k := ScopeKey{Type: action, Scope: scope}
		t := tallies[k]
		t.Total++
		if passed {
			t.Green++
			if e.Time.After(t.LastGreen) {
				t.LastGreen = e.Time
			}
		}
		tallies[k] = t
	}
	if err := sc.Err(); err != nil {
		return TrustView{tallies: map[ScopeKey]Tally{}}, fmt.Errorf("graapprove: reading event log: %w", err)
	}

	// Chain integrity is eventlog's authority, not ours: a parseable log whose
	// hashes do not link is untrustworthy and must surface as an error AFTER we drop
	// the tallies we just built from it. Empty view + ChainOK=false + the error so
	// the caller denies explicitly (fail-closed — earn nothing over a tampered
	// chain).
	if err := eventlog.Verify(logPath); err != nil {
		return TrustView{tallies: map[ScopeKey]Tally{}, ChainOK: false}, fmt.Errorf("graapprove: verifying chain: %w", err)
	}
	return TrustView{tallies: tallies, ChainOK: true}, nil
}
