package graapprove

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
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
//
// The caller passes the CONCRETE scope of the action it is deciding (a real branch);
// the lookup normalizes it to the stable family the tallies are bucketed under, so a
// per-run-unique branch still finds the history its family earned. trustScope is
// idempotent, so passing an already-normalized key is also correct.
func (v TrustView) Tally(k ScopeKey) Tally {
	if v.tallies == nil {
		return Tally{}
	}
	k.Scope = trustScope(k.Scope)
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
	tallies, _, err := foldTallies(logPath, 0, map[ScopeKey]Tally{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No log yet ⇒ no history ⇒ a clean empty, trusted-shape view.
			return TrustView{tallies: map[ScopeKey]Tally{}, ChainOK: true}, nil
		}
		return TrustView{tallies: map[ScopeKey]Tally{}}, err
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

// foldTallies folds every boundary_outcome line at logPath at or after byte offset
// `from` INTO the caller-supplied tallies map (mutated in place) and returns it along
// with the byte offset one past the last COMPLETE line consumed. The offset lets a
// memoizing caller (TrustBuilder) resume folding only the appended suffix on the next
// call rather than rescanning the whole log. `from` MUST fall on a line boundary (0,
// or a value previously returned by foldTallies) so no partial line is ever misread;
// TrustBuilder only ever passes such offsets. A missing log surfaces fs.ErrNotExist so
// BuildTrust can treat it as a clean empty view.
func foldTallies(logPath string, from int64, tallies map[ScopeKey]Tally) (map[ScopeKey]Tally, int64, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tallies, from, err // propagated; BuildTrust maps it to a clean empty view
		}
		return map[ScopeKey]Tally{}, 0, fmt.Errorf("graapprove: opening event log: %w", err)
	}
	defer f.Close()

	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return map[ScopeKey]Tally{}, 0, fmt.Errorf("graapprove: seeking event log: %w", err)
		}
	}

	consumed := from
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		// Advance the consumed watermark by this line plus its trailing newline. Every
		// event Append writes exactly one "<json>\n", so a scanned line is always
		// followed by a newline in the durable file; this keeps `consumed` on a line
		// boundary for the next incremental fold.
		consumed += int64(len(line)) + 1
		if len(line) == 0 {
			continue
		}
		var e boundaryEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return map[ScopeKey]Tally{}, 0, fmt.Errorf("graapprove: event %d: parsing line: %w", n, err)
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

		// Tally against the scope FAMILY, not the concrete branch: the event records a
		// per-run-unique scope ("task/trig-<nano>", an integration tip), so an
		// exact-scope bucket would hold at most one outcome and never earn trust. The
		// GradedApprover looks up the same family key.
		k := ScopeKey{Type: action, Scope: trustScope(scope)}
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
		return map[ScopeKey]Tally{}, 0, fmt.Errorf("graapprove: reading event log: %w", err)
	}
	return tallies, consumed, nil
}

// TrustBuilder is a memoizing, incremental front for BuildTrust. Rebuilding trust on
// every gate decision otherwise re-scans AND re-Verifies the WHOLE append-only log
// (O(log) per approval); as the log grows, each decision pays for all history. The
// builder folds only the appended suffix past a cached byte offset and, when the log
// has not changed at all since the last call (same size + mtime), returns the cached
// view WITHOUT re-scanning or re-Verifying. Correctness is preserved: the tallies are
// identical to a full BuildTrust, and any growth still re-runs eventlog.Verify over
// the whole chain (chain integrity is eventlog's authority and cannot be made partial
// from this package). A detected shrink/rewrite (size < cached, or the file
// disappeared) invalidates the cache and folds from scratch, so a truncation or
// tamper can never leave stale trust behind.
//
// A zero-value TrustBuilder is ready to use; Build is safe for concurrent callers (the
// autonomy daemon / swarm can share one approver over one log). A nil *TrustBuilder
// falls back to a plain BuildTrust so an unwired path is byte-identical.
type TrustBuilder struct {
	mu       sync.Mutex
	cached   map[ScopeKey]Tally // folded tallies as of `offset`
	offset   int64              // byte offset one past the last complete folded line
	size     int64              // file size at the last successful fold
	modTime  time.Time          // file mtime at the last successful fold
	verified bool               // whether the cached prefix passed eventlog.Verify
}

// Build returns the current TrustView, folding only new bytes since the last call and
// skipping the scan+Verify entirely when the log is byte-for-byte unchanged.
func (b *TrustBuilder) Build(logPath string) (TrustView, error) {
	if b == nil {
		return BuildTrust(logPath) // unwired: byte-identical to the non-memoized path
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	fi, statErr := os.Stat(logPath)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			// Log gone (never created, or removed). Reset the cache and report the clean
			// empty, trusted-shape view — a later re-created log folds from scratch.
			b.reset()
			return TrustView{tallies: map[ScopeKey]Tally{}, ChainOK: true}, nil
		}
		return TrustView{tallies: map[ScopeKey]Tally{}}, fmt.Errorf("graapprove: stat event log: %w", statErr)
	}

	// Fast path: the file is byte-for-byte unchanged since the last successful build
	// (same size AND mtime). No scan, no Verify — just re-emit the cached, already-
	// verified view. If the previous build had NOT verified (a broken chain), we must
	// NOT serve trust from cache: fall through and re-fold+verify so the deny persists.
	if b.cached != nil && b.verified && fi.Size() == b.size && fi.ModTime().Equal(b.modTime) {
		return TrustView{tallies: copyTallies(b.cached), ChainOK: true}, nil
	}

	// A shrink or rewrite (size below our watermark) means the prefix we folded is no
	// longer a prefix — invalidate and fold from scratch so stale trust never survives
	// a truncation/tamper.
	from := b.offset
	var base map[ScopeKey]Tally
	if b.cached == nil || fi.Size() < b.offset {
		from = 0
		base = map[ScopeKey]Tally{}
	} else {
		base = copyTallies(b.cached) // fold onto a copy so a mid-fold error leaves the cache intact
	}

	tallies, consumed, err := foldTallies(logPath, from, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			b.reset()
			return TrustView{tallies: map[ScopeKey]Tally{}, ChainOK: true}, nil
		}
		return TrustView{tallies: map[ScopeKey]Tally{}}, err
	}

	// Chain integrity: re-Verify the whole chain (authority lives in eventlog). A
	// broken chain drops all tallies and is NOT cached as verified, so the next
	// decision re-checks rather than trusting a tampered prefix.
	if verr := eventlog.Verify(logPath); verr != nil {
		b.cached, b.offset, b.size, b.modTime, b.verified = tallies, consumed, fi.Size(), fi.ModTime(), false
		return TrustView{tallies: map[ScopeKey]Tally{}, ChainOK: false}, fmt.Errorf("graapprove: verifying chain: %w", verr)
	}

	b.cached, b.offset, b.size, b.modTime, b.verified = tallies, consumed, fi.Size(), fi.ModTime(), true
	return TrustView{tallies: copyTallies(tallies), ChainOK: true}, nil
}

// reset clears the memoized state so the next Build folds from scratch.
func (b *TrustBuilder) reset() {
	b.cached, b.offset, b.size, b.modTime, b.verified = nil, 0, 0, time.Time{}, false
}

// copyTallies returns a shallow copy of a tallies map (Tally is a value type), so the
// TrustView handed to a caller can never be mutated by a later incremental fold onto
// the builder's own cache.
func copyTallies(src map[ScopeKey]Tally) map[ScopeKey]Tally {
	dst := make(map[ScopeKey]Tally, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
