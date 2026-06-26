package vcache

import (
	"bufio"
	"encoding/json"
	"os"

	"nilcore/internal/eventlog"
)

// cacheRecord mirrors only the fields of an on-disk cache event the lookup reads:
// the kind, and the structural Detail the key match needs. Every other field
// (time, seq, prev, hash, …) is intentionally ignored here — chain integrity is
// eventlog.Verify's job, run separately below, never re-implemented in this fold.
type cacheRecord struct {
	Kind   string         `json:"kind"`
	Detail map[string]any `json:"detail"`
}

// lookup reports whether a prior, ORIGINAL (non-replay) cache PASS exists for key
// in a CHAIN-VERIFIED log. It is the I2-load-bearing gate of the whole package, so
// it is deliberately fail-closed at every step:
//
//   - A missing log ⇒ no history ⇒ no hit (false), no error. A fresh worktree has
//     simply never passed this check yet, which must recompute.
//   - A read error, an unparseable line, or ANY OTHER open failure ⇒ no hit. We
//     can never serve a cached pass over a log we could not fully read.
//   - The chain is verified LAST, with eventlog.Verify, and a chain error ⇒ no hit
//     (fail-closed-to-recompute, the review's I2 fix). A tampered, reordered,
//     dropped, or corrupt log can never short-circuit the verifier — even if a
//     matching line is physically present, a broken chain discards the match.
//
// Only an ORIGINAL pass (replay=false) is matchable: a served hit is itself logged
// (for audit) but must never seed further hits, so the cache cannot bootstrap a
// pass from its own replay — every hit traces back to one real inner-verifier
// verdict over the verified chain.
func (c *Cache) lookup(key string) bool {
	f, err := os.Open(c.cfg.LogPath)
	if err != nil {
		// No log yet ⇒ no earned pass. Any other open error (permissions, an I/O
		// fault) is also fail-closed: we recompute rather than guess a hit.
		return false
	}
	matched := scanForKey(f, key)
	f.Close()
	if !matched {
		return false
	}

	// A matching line exists — but it is only trustworthy if the WHOLE chain
	// verifies. eventlog.Verify re-reads the file end to end (sequence anchor, prev
	// links, keyed hashes); on ANY error we discard the match and recompute. This is
	// the fail-closed-to-recompute rule: a cached pass may ONLY ever come from a
	// chain-verified log.
	if err := eventlog.Verify(c.cfg.LogPath); err != nil {
		return false
	}
	return true
}

// scanForKey reports whether the log stream carries an original cache PASS for key.
// It reads read-only and tolerates nothing silently: an unparseable line aborts the
// scan as a no-match (fail-closed) rather than skipping to a later, possibly
// attacker-positioned, matching line.
func scanForKey(f *os.File, key string) bool {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r cacheRecord
		if err := json.Unmarshal(line, &r); err != nil {
			// A corrupt line means we cannot trust our read of the file; do not keep
			// scanning for a "good" match past it. eventlog.Verify would reject this
			// log anyway, but failing the scan here is the same fail-closed posture.
			return false
		}
		if r.Kind != kindCachePass {
			continue
		}
		if recordMatches(r.Detail, key) {
			return true
		}
	}
	// No matching line. A scan error (e.g. an over-long line) lands here too, which
	// is the correct fail-closed posture: no confirmed match, and eventlog.Verify
	// would reject the same log anyway.
	return false
}

// recordMatches reports whether a cache event's Detail is an ORIGINAL pass for key.
// All three structural conditions must hold: the recorded key equals key, the
// verdict is a true pass, and it is not itself a replay. A missing or wrong-typed
// field reads as a non-match (fail-safe: absent evidence is never a hit).
func recordMatches(detail map[string]any, key string) bool {
	if detail == nil {
		return false
	}
	gotKey, _ := detail[detailFieldDigest].(string)
	if gotKey != key {
		return false
	}
	passed, _ := detail[detailFieldPassed].(bool)
	if !passed {
		return false
	}
	// "replay" defaults to false when absent (older/original entries), which is the
	// matchable case. Only an explicitly-true replay is excluded.
	replay, _ := detail["replay"].(bool)
	return !replay
}
