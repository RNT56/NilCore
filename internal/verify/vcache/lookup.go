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
	matched, corrupt := scanForKey(f, key)
	f.Close()
	if corrupt {
		// An unparseable line aborts the scan (and eventlog.Verify would reject the same
		// log), so the cache will permanently recompute. Emit ONE diagnostic so an operator
		// notices a poisoned-cache condition rather than a silent perpetual miss. This
		// changes no verdict — we still fall through to recompute below.
		c.emitCorruptDiagnostic()
	}
	if !matched {
		return false
	}

	// A matching line exists — but it is only trustworthy if the WHOLE chain
	// verifies. verifyChain runs eventlog.Verify (sequence anchor, prev links, keyed
	// hashes) end to end, MEMOIZED by a cheap file fingerprint (size + mtime): an
	// unchanged file is still the chain-verified file it was, so a run of hits over the
	// same log verifies exactly once instead of O(hits) full re-hashes. On ANY error we
	// discard the match and recompute — the fail-closed-to-recompute rule: a cached pass
	// may ONLY ever come from a chain-verified log.
	return c.verifyChain()
}

// verifyChain reports whether the log at LogPath is chain-verified, memoizing the
// result by a cheap fingerprint of the file (size + mod-time). Trusting the memo is
// SOUND because it is keyed on a fingerprint that changes on ANY mutation: an append
// grows the size, an in-place edit (a tamper) bumps the mtime — either misses the memo
// and pays a full fresh eventlog.Verify. So chain integrity is never assumed across a
// change, only re-affirmed for free across a genuinely-unchanged file. A stat error or
// a failing Verify is fail-closed (false) and is NOT memoized as a pass.
func (c *Cache) verifyChain() bool {
	print, err := statPrint(c.cfg.LogPath)
	if err != nil {
		return false
	}

	c.mu.Lock()
	if c.haveVerifyMemo && c.verifiedPrint == print {
		ok := c.verifiedOK
		c.mu.Unlock()
		return ok
	}
	c.mu.Unlock()

	ok := eventlog.Verify(c.cfg.LogPath) == nil

	c.mu.Lock()
	// Re-stat guard: only record the memo against the fingerprint we actually verified.
	// If the file changed during Verify (a concurrent append/edit), the print no longer
	// describes what Verify read, so leave the memo untouched (the next lookup
	// re-verifies) rather than caching a verdict against a moved target.
	if print2, serr := statPrint(c.cfg.LogPath); serr == nil && print2 == print {
		c.verifiedPrint = print
		c.verifiedOK = ok
		c.haveVerifyMemo = true
	}
	c.mu.Unlock()
	return ok
}

// statPrint returns the change-detection fingerprint (size + mod-time) of the file
// at path.
func statPrint(path string) (filePrint, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return filePrint{}, err
	}
	return filePrint{size: fi.Size(), mtime: fi.ModTime().UnixNano()}, nil
}

// scanForKey reports whether the log stream carries an original cache PASS for key.
// It reads read-only and tolerates nothing silently: an unparseable line aborts the
// scan as a no-match (fail-closed) rather than skipping to a later, possibly
// attacker-positioned, matching line. The second return value (corrupt) is true when
// the scan aborted on an unparseable line, so the caller can emit a one-time
// poisoned-cache diagnostic — it never changes the match result.
func scanForKey(f *os.File, key string) (matched, corrupt bool) {
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
			// Surface the corruption so the caller can emit a one-time diagnostic.
			return false, true
		}
		if r.Kind != kindCachePass {
			continue
		}
		if recordMatches(r.Detail, key) {
			return true, false
		}
	}
	// No matching line. A scan error (e.g. an over-long line) lands here too, which
	// is the correct fail-closed posture: no confirmed match, and eventlog.Verify
	// would reject the same log anyway.
	return false, false
}

// emitCorruptDiagnostic appends ONE kindCacheCorrupt event (guarded by corruptOnce) so
// an operator notices a poisoned/permanent-recompute cache. It is fire-and-forget
// through the nil-safe Append; a nil Log records nothing. The detail carries only
// structural metadata (the verifier id / toolchain) — never raw log bytes (I7).
func (c *Cache) emitCorruptDiagnostic() {
	if c == nil || c.cfg.Log == nil {
		return
	}
	c.corruptOnce.Do(func() {
		c.cfg.Log.Append(eventlog.Event{
			Task: c.cfg.Task,
			Kind: kindCacheCorrupt,
			Detail: map[string]any{
				"reason":      "unparseable cache line; cache will recompute until the log is repaired",
				"verifier_id": c.cfg.VerifierID,
				"toolchain":   c.cfg.Toolchain,
			},
		})
	})
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
