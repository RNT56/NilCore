// Package vcache is the A9 content-hash verification cache (Pillar 3, LRN-T04):
// a decorator that skips a verifier run when the EXACT same check has already
// produced a chain-verified PASS over the EXACT same worktree.
//
// WHY. A full `make verify` is the most expensive step in the loop, and the loop
// re-runs it constantly — after every edit, on every requeue, across swarm
// children that converge on the same tip. When nothing the check reads has
// changed, re-running it buys nothing but wall-clock. So this package keys a pass
// on (worktree-content-hash + verifier-id + toolchain-version) and, on a key it
// has seen pass before, returns that pass without re-running.
//
// WHY it is still I2-safe. The verifier remains the sole authority on "done"
// (CLAUDE.md §2 I2). A cached pass is never a self-report and never a fresh claim:
// it is a REPLAY of a verdict the inner verifier itself produced earlier, recorded
// in the append-only hash-chained event log. Three disciplines keep that honest:
//
//   - The cache only ever records a pass AFTER the inner verifier returns one. A
//     failure is never cached, and the cache never invents a verdict.
//   - Lookup MUST run eventlog.Verify and FAIL-CLOSED-TO-RECOMPUTE on ANY chain
//     error (review's I2 fix): a tampered, reordered, dropped, or corrupt log can
//     never serve a cached pass. A broken chain forces the inner verifier to run.
//   - The key includes the verifier id, its config, AND the toolchain version, so
//     a *different* check (a different command, a different Go) can never collide
//     onto a stale entry. The content hash covers everything the check reads, so a
//     changed worktree never hits a prior key.
//
// WHY it is default-off / byte-identical when unwired. A nil *Cache delegates to
// the inner verifier exactly as today; Decorate(nil-ish config) returns the inner
// verifier unwrapped. Nothing about the verdict path changes unless the operator
// opts in (NILCORE_VCACHE, wired in LRN-T05). The package is conservative: when in
// any doubt it recomputes — the cache can only ever make the loop faster, never
// change what "done" means.
//
// I7: untrusted/distilled text never reaches this package. The key is built only
// from structural, self-produced values (a content hash, an id, a version string);
// no file CONTENT and no verifier OUTPUT is interpreted as an instruction — output
// is hashed as opaque bytes, nothing more.
package vcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"nilcore/internal/eventlog"
	"nilcore/internal/verify"
)

// kindCachePass is the event kind a cached-eligible PASS is recorded under. It is
// distinct from the backend's own "verify" event so the cache reads only verdicts
// IT produced (each carrying a cache_key), never another producer's verify event.
const kindCachePass = "verify_cache"

// kindCacheCorrupt is the one-time DIAGNOSTIC event emitted when the lookup scan hits
// an unparseable line in the log. Such a line makes the scan (and eventlog.Verify
// itself) fail-closed, so the cache silently degrades to a PERMANENT recompute with no
// other signal. Emitting one diagnostic lets an operator notice a poisoned-cache
// condition rather than puzzle over a cache that never hits. It changes no verdict (a
// corrupt log still recomputes); it is observability only.
const kindCacheCorrupt = "verify_cache_corrupt"

// detailFieldDigest / detailFieldPassed are the structural Detail fields a cache
// event carries. Only these decide a hit — never any free-text output.
//
// NOTE the field name is "cache_digest", NOT "cache_key": the event log redacts any
// Detail field whose NAME contains "key"/"token"/"secret" (eventlog.isSecretKey), so
// a "*_key" field would be written as "[redacted]" and never match on lookup. The
// value is a non-secret content hash regardless; the digest naming sidesteps the
// name-based redactor entirely.
const (
	detailFieldDigest = "cache_digest"
	detailFieldPassed = "passed"
)

// Hasher computes a content hash over everything the verifier reads — by default
// the whole worktree tree (the production wiring injects verify.ContentHashWorktree).
// It is injected so tests stay
// hermetic and so a wiring layer can widen the hashed surface (e.g. fold in
// pinned-tool digests) without this package importing the file system walker
// directly. It takes ctx first and honors cancellation.
type Hasher func(ctx context.Context) (string, error)

// Config is the wiring a Cache needs. All fields are required for a live cache;
// New rejects a config missing any of them rather than silently degrading to a
// cache that can never hit (fail-loud over fail-silent).
type Config struct {
	// Inner is the verifier being decorated — the sole authority on "done". A hit
	// replays its prior verdict; a miss delegates to it unchanged.
	Inner verify.Verifier

	// Log is the append-only hash-chained log. The cache READS LogPath for prior
	// passes and APPENDS its own cache events here. Append is nil-safe, so a nil Log
	// simply records nothing (and therefore never produces a future hit) — the cache
	// still functions, just without persisting new entries.
	Log *eventlog.Log

	// LogPath is the on-disk path of Log, read read-only during Lookup and handed to
	// eventlog.Verify. It MUST point at the same file Log writes, or a hit could be
	// served over an unverified chain — so New requires it explicitly.
	LogPath string

	// Hash computes the worktree content hash. Required.
	Hash Hasher

	// VerifierID identifies the check whose verdict is being cached — e.g. its
	// command string or a stable label. It is folded into the key so a different
	// verifier never reuses this one's pass.
	VerifierID string

	// Toolchain is the toolchain version (e.g. runtime.Version() + the project's
	// pinned tool digests). Folded into the key so a toolchain bump invalidates every
	// prior pass — a green under Go 1.25 is not evidence of green under Go 1.26.
	Toolchain string

	// Task is the optional task id stamped onto recorded cache events for audit
	// correlation. It is NOT part of the key (a pass is keyed by content+check, not
	// by which task produced it).
	Task string
}

// Cache decorates a verify.Verifier with a content-hash pass cache. The zero value
// is not usable; construct it with New. A nil *Cache is valid and delegates (see
// Check), which is what keeps the feature byte-identical when unwired.
type Cache struct {
	cfg Config
	// corruptOnce guards the one-time poisoned-cache diagnostic (kindCacheCorrupt), so a
	// repeatedly-scanned corrupt log emits a single signal, never a flood of identical
	// events. It is presentational/observability only — it never affects a verdict.
	corruptOnce sync.Once

	// verifyMemo caches the result of the last successful eventlog.Verify keyed by a
	// cheap fingerprint of the log file (its byte size AND its mod-time). This is what
	// stops every cache hit from re-reading and re-hashing the whole log (the O(n^2)
	// trap): the first lookup after a change pays the full eventlog.Verify; repeated
	// lookups over the UNCHANGED log serve the memoized verdict for free.
	//
	// Soundness: the memo is trusted ONLY when the fingerprint is byte-identical to the
	// one that verified. An append grows the size; an in-place edit (a tamper) bumps the
	// mod-time — either misses the memo and forces a fresh full Verify, so the chain
	// guarantee is re-affirmed for free across a genuinely-unchanged file but never
	// ASSUMED across a mutation. It is a pure optimization: worst case it degrades to the
	// old behavior (verify every lookup), it can never serve a hit the full Verify would
	// have rejected. mu guards it against concurrent Checks.
	mu             sync.Mutex
	verifiedPrint  filePrint
	verifiedOK     bool
	haveVerifyMemo bool
}

// filePrint is a cheap change-detector for the log file: its byte size plus its
// mod-time (Unix nanoseconds). An append changes size; an in-place rewrite/tamper
// changes mtime. Equal prints ⇒ the file has not changed since it was last verified.
type filePrint struct {
	size  int64
	mtime int64
}

// New builds a Cache from cfg, validating that every field a live cache needs is
// present. It fails loud on an incomplete config: a half-wired cache that can
// never hit is a silent performance bug, and worse, a LogPath that disagrees with
// Log would be a correctness/I2 hazard. Inner being nil is the one thing we cannot
// recover from at Check time, so it is rejected here too.
func New(cfg Config) (*Cache, error) {
	if cfg.Inner == nil {
		return nil, fmt.Errorf("vcache: Inner verifier is required")
	}
	if cfg.Hash == nil {
		return nil, fmt.Errorf("vcache: Hash function is required")
	}
	if cfg.LogPath == "" {
		return nil, fmt.Errorf("vcache: LogPath is required")
	}
	if cfg.VerifierID == "" {
		return nil, fmt.Errorf("vcache: VerifierID is required")
	}
	if cfg.Toolchain == "" {
		return nil, fmt.Errorf("vcache: Toolchain is required")
	}
	return &Cache{cfg: cfg}, nil
}

// Decorate wraps inner in a content-hash cache, or returns inner UNCHANGED when
// the cache cannot be built (any construction error, or a nil inner). This is the
// default-off seam the wiring layer calls: when NILCORE_VCACHE is off it passes a
// config that fails New (or simply calls Inner directly), and the verifier runs
// exactly as today. The returned Verifier is always safe to call.
func Decorate(cfg Config) verify.Verifier {
	c, err := New(cfg)
	if err != nil {
		return cfg.Inner // byte-identical fallback (may itself be nil; caller's contract)
	}
	return c
}

// Check is the verify.Verifier implementation. It is nil-safe: a nil *Cache means
// "no cache wired", so it delegates to nothing of its own — there is no inner to
// call, so a nil receiver returns a zero Report and a nil error only if there is
// genuinely nothing to do. In practice a nil *Cache is never stored as the active
// verifier; the unwrapped inner verifier is used instead (see Decorate). The
// nil-receiver guard exists so a stray nil never panics.
func (c *Cache) Check(ctx context.Context) (verify.Report, error) {
	if c == nil {
		// No cache: there is nothing to delegate to from here. The wiring contract
		// (Decorate) never installs a nil *Cache as the verifier — it installs the
		// inner verifier directly — so this path is defensive only.
		return verify.Report{}, fmt.Errorf("vcache: Check on a nil cache")
	}

	// Compute the key. ANY failure to compute it (e.g. the worktree could not be
	// hashed) is conservative: we cannot prove a cached pass applies, so we recompute
	// by delegating to the inner verifier — never serve a guessed hit.
	key, err := c.key(ctx)
	if err != nil {
		return c.cfg.Inner.Check(ctx)
	}

	// Lookup is the I2-critical step: it returns a hit ONLY from a chain-verified
	// log. On any chain error it returns hit=false (fail-closed-to-recompute), so a
	// broken/tampered chain can never short-circuit the verifier.
	//
	// A HIT appends NOTHING: a served replay is not new history, and recording one
	// per hit both grew the append-only log without bound and forced the very next
	// lookup to re-verify a longer chain — the O(n^2) trap the memo above closes. Only
	// a genuine cache WRITE (an original inner-verifier pass, below) is recorded.
	if hit := c.lookup(key); hit {
		return verify.Report{
			Passed: true,
			Output: "verify cache: replayed a chain-verified pass (key " + short(key) + ")",
		}, nil
	}

	// Miss: the inner verifier — the sole authority — decides. Only a genuine PASS is
	// recorded as a future-eligible cache entry; a failure (or an error) is never
	// cached, so a red check can never be replayed as green.
	rep, err := c.cfg.Inner.Check(ctx)
	if err != nil {
		return rep, err
	}
	if rep.Passed {
		c.record(key, true, false)
	}
	return rep, nil
}

// key computes the cache key = sha256(verifierID ∥ toolchain ∥ contentHash), each
// field length-prefixed so no two distinct field tuples can collide by being run
// together. Folding the verifier id and toolchain in is what stops a DIFFERENT
// check ever hitting THIS check's pass; the content hash is what stops a changed
// worktree hitting a stale pass.
func (c *Cache) key(ctx context.Context) (string, error) {
	content, err := c.cfg.Hash(ctx)
	if err != nil {
		return "", fmt.Errorf("hashing worktree: %w", err)
	}
	h := sha256.New()
	writeField(h, "vid", c.cfg.VerifierID)
	writeField(h, "tc", c.cfg.Toolchain)
	writeField(h, "content", content)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// record appends a cache event so an ORIGINAL inner-verifier pass is auditable and
// eligible for a future hit. It is fire-and-forget through the nil-safe Append; a
// nil Log records nothing. A served hit is NOT recorded (see Check): only a genuine
// cache write reaches here, so `replay` is always false — the field is still written
// so recordMatches (which excludes replay=true) stays forward-compatible with any
// externally-produced replay entry it might encounter.
func (c *Cache) record(key string, passed, replay bool) {
	if c.cfg.Log == nil {
		return
	}
	c.cfg.Log.Append(eventlog.Event{
		Task: c.cfg.Task,
		Kind: kindCachePass,
		Detail: map[string]any{
			detailFieldDigest: key,
			detailFieldPassed: passed,
			"replay":          replay,
			"verifier_id":     c.cfg.VerifierID,
			"toolchain":       c.cfg.Toolchain,
		},
	})
}

// short returns a stable, log-friendly prefix of a hex key for human-readable
// audit lines. It is presentational only and never used for matching.
func short(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

// writeField length-prefixes a labeled field into h so concatenation is injective
// (no "ab"+"c" == "a"+"bc" collision across fields).
func writeField(h interface{ Write([]byte) (int, error) }, label, val string) {
	fmt.Fprintf(h, "%s:%d:", label, len(val))
	_, _ = h.Write([]byte(val))
	_, _ = h.Write([]byte{0})
}
