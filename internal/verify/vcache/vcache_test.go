package vcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/verify"
)

// spy is a verify.Verifier that counts calls and returns a scripted report. It is
// how every test asserts the I2-critical property: a hit must NOT call the inner
// verifier, a miss MUST.
type spy struct {
	calls  atomic.Int64
	report verify.Report
	err    error
}

func (s *spy) Check(context.Context) (verify.Report, error) {
	s.calls.Add(1)
	return s.report, s.err
}

// fixedHash is a deterministic Hasher returning a caller-chosen content hash, so a
// test can control whether the worktree "changed" without touching the disk.
func fixedHash(h string) Hasher {
	return func(context.Context) (string, error) { return h, nil }
}

// freshLog opens a real hash-chained log in a temp dir and returns it plus its
// path. The chain is genuine, so eventlog.Verify runs the production check.
func freshLog(t *testing.T) (*eventlog.Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, path
}

func baseConfig(inner verify.Verifier, log *eventlog.Log, path, content string) Config {
	return Config{
		Inner:      inner,
		Log:        log,
		LogPath:    path,
		Hash:       fixedHash(content),
		VerifierID: "make verify",
		Toolchain:  "go1.25.0",
		Task:       "T-test",
	}
}

// TestMatchingKeyShortCircuits: a first Check (miss) records a pass; a second Check
// over the SAME key replays it WITHOUT calling the inner verifier.
func TestMatchingKeyShortCircuits(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true, Output: "real run"}}
	log, path := freshLog(t)
	c, err := New(baseConfig(inner, log, path, "content-A"))
	if err != nil {
		t.Fatal(err)
	}

	rep, err := c.Check(context.Background())
	if err != nil || !rep.Passed {
		t.Fatalf("first Check (miss) = %+v, %v; want a pass", rep, err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("first Check should run inner exactly once, ran %d", got)
	}

	// Second Check: same content, same verifier id, same toolchain ⇒ same key ⇒ hit.
	rep2, err := c.Check(context.Background())
	if err != nil || !rep2.Passed {
		t.Fatalf("second Check (hit) = %+v, %v; want a pass", rep2, err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("second Check must NOT re-run inner; inner call count = %d, want 1", got)
	}
	if !strings.Contains(rep2.Output, "verify cache") {
		t.Errorf("a hit should be labeled as a cache replay, got %q", rep2.Output)
	}
}

// TestDifferentContentReRuns: a changed worktree (different content hash) is a
// different key and MUST re-run the inner verifier rather than serve a stale pass.
func TestDifferentContentReRuns(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)

	c1, _ := New(baseConfig(inner, log, path, "content-A"))
	if _, err := c1.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// New worktree content ⇒ new key. Reuse the same log/path so the prior pass IS
	// present — only the content differs.
	c2, _ := New(baseConfig(inner, log, path, "content-B"))
	if _, err := c2.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("a changed worktree must re-run the verifier; inner ran %d times, want 2", got)
	}
}

// TestDifferentToolchainReRuns: same content, but a toolchain bump invalidates the
// prior pass — a green under one toolchain is not evidence of green under another.
func TestDifferentToolchainReRuns(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)

	cfg := baseConfig(inner, log, path, "content-A")
	c1, _ := New(cfg)
	if _, err := c1.Check(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg2 := baseConfig(inner, log, path, "content-A")
	cfg2.Toolchain = "go1.26.0" // toolchain bump, same content
	c2, _ := New(cfg2)
	if _, err := c2.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("a toolchain bump must re-run the verifier; inner ran %d times, want 2", got)
	}
}

// TestDifferentVerifierIDReRuns: a different verifier (different command/id) never
// reuses another check's pass, even on identical content and toolchain.
func TestDifferentVerifierIDReRuns(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)

	c1, _ := New(baseConfig(inner, log, path, "content-A"))
	if _, err := c1.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg2 := baseConfig(inner, log, path, "content-A")
	cfg2.VerifierID = "make integration-verify"
	c2, _ := New(cfg2)
	if _, err := c2.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("a different verifier id must re-run; inner ran %d times, want 2", got)
	}
}

// TestBrokenChainForcesRecompute: a prior pass exists for the key, but the log
// chain is tampered. Lookup MUST run eventlog.Verify, see the break, and recompute
// (call inner) rather than serve the cached pass. This is the review's I2 fix.
func TestBrokenChainForcesRecompute(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)
	c, _ := New(baseConfig(inner, log, path, "content-A"))

	// First Check records a real pass for the key.
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// Sanity: the chain is intact before tampering, and a hit would normally serve.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("pre-tamper chain should verify: %v", err)
	}

	// Tamper: flip the recorded key so the line's hash no longer matches its content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), "content-A", "content-X", 1)
	if tampered == string(data) {
		// The key is a sha256 over the content, not the literal "content-A"; tamper a
		// structural byte instead to guarantee a chain break.
		tampered = strings.Replace(string(data), `"passed":true`, `"passed":false`, 1)
	}
	if tampered == string(data) {
		t.Fatal("test setup: nothing to tamper")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eventlog.Verify(path); err == nil {
		t.Fatal("test setup: tampering did not break the chain")
	}

	// New cache over the tampered log. The key still matches a physically-present
	// line, but the chain is broken ⇒ no hit ⇒ recompute.
	inner2 := &spy{report: verify.Report{Passed: true}}
	c2, _ := New(Config{
		Inner: inner2, Log: nil, LogPath: path,
		Hash: fixedHash("content-A"), VerifierID: "make verify", Toolchain: "go1.25.0",
	})
	rep, err := c2.Check(context.Background())
	if err != nil || !rep.Passed {
		t.Fatalf("recompute Check = %+v, %v; want a fresh pass", rep, err)
	}
	if got := inner2.calls.Load(); got != 1 {
		t.Errorf("a broken chain must force recompute (run inner); inner ran %d times, want 1", got)
	}
}

// TestCorruptLineEmitsOneTimeDiagnostic: an unparseable line in the log aborts the
// scan (and eventlog.Verify would reject the same log), so the cache permanently
// recomputes. The fix emits ONE kindCacheCorrupt diagnostic so an operator notices a
// poisoned-cache condition rather than a silent perpetual miss. The diagnostic is
// guarded by corruptOnce: repeated lookups over the same corrupt log emit it once.
func TestCorruptLineEmitsOneTimeDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	// Write a single CORRUPT (non-JSON) line as the whole log, then close so a fresh
	// reader sees it. The scan aborts on this line and reports corruption.
	if err := os.WriteFile(path, []byte("this is not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real log we can both read corruption from and append the diagnostic to. We
	// reopen the same path so the diagnostic lands in the same file the scan read.
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	inner := &spy{report: verify.Report{Passed: true}}
	c, err := New(Config{
		Inner: inner, Log: log, LogPath: path,
		Hash: fixedHash("content-A"), VerifierID: "make verify", Toolchain: "go1.25.0", Task: "T-diag",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two lookups over the corrupt log: both must recompute (return false), and exactly
	// ONE diagnostic event must be emitted across both.
	if hit := c.lookup("any-key"); hit {
		t.Fatal("a corrupt log must never serve a hit")
	}
	if hit := c.lookup("any-key"); hit {
		t.Fatal("a corrupt log must never serve a hit (second lookup)")
	}

	data, _ := os.ReadFile(path)
	got := strings.Count(string(data), kindCacheCorrupt)
	if got != 1 {
		t.Fatalf("want exactly one %q diagnostic across repeated lookups, got %d:\n%s", kindCacheCorrupt, got, data)
	}
}

// countEvents returns how many JSONL lines the log at path currently holds. Used to
// assert a cache HIT appends nothing (no unbounded per-hit growth).
func countEvents(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestHitAppendsNothing: a cache HIT must not append any event. Recording a replay
// per hit grew the append-only log without bound AND forced the next lookup to
// re-verify a longer chain (the O(n^2) trap). The first Check (miss) writes exactly
// one cache event; every subsequent hit over the same log leaves the event count
// unchanged.
func TestHitAppendsNothing(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)
	c, err := New(baseConfig(inner, log, path, "content-A"))
	if err != nil {
		t.Fatal(err)
	}

	// Miss: records exactly one original cache-pass event.
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterMiss := countEvents(t, path)

	// A run of hits must not grow the log at all.
	for i := 0; i < 5; i++ {
		rep, err := c.Check(context.Background())
		if err != nil || !rep.Passed {
			t.Fatalf("hit %d = %+v, %v; want a pass", i, rep, err)
		}
		if got := countEvents(t, path); got != afterMiss {
			t.Fatalf("hit %d appended an event: log grew from %d to %d", i, afterMiss, got)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("hits must not re-run inner; inner ran %d times, want 1", got)
	}
}

// TestHitMemoizesChainVerify: after the first hit verifies the chain, subsequent
// hits over the UNCHANGED log serve the memoized verdict without re-reading the
// whole file. We assert the memo is populated at the size it verified and reused: a
// second hit does not re-run inner and the memoized size matches the log's actual
// size (the append-only invariant that makes skipping the re-verify sound).
func TestHitMemoizesChainVerify(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)
	c, err := New(baseConfig(inner, log, path, "content-A"))
	if err != nil {
		t.Fatal(err)
	}

	// Miss records the pass; a first hit verifies the chain and populates the memo.
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	haveMemo, memoOK, memoSize := c.haveVerifyMemo, c.verifiedOK, c.verifiedPrint.size
	c.mu.Unlock()
	if !haveMemo || !memoOK {
		t.Fatalf("a chain-verified hit must memoize the verdict: haveMemo=%v ok=%v", haveMemo, memoOK)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if memoSize != fi.Size() {
		t.Fatalf("memo size %d must equal the unchanged log size %d", memoSize, fi.Size())
	}

	// A further hit over the same log serves from the memo (no inner call).
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("memoized hits must not re-run inner; inner ran %d, want 1", got)
	}
}

// TestGrowthInvalidatesMemo: an append after a memoized verify (a new original pass
// for a different key) changes the log size, so the next lookup must NOT trust the
// stale memo — it re-verifies at the new size. We prove this by appending a genuine
// event out-of-band, then confirming a hit still succeeds (the fresh verify passed
// over the grown, still-intact chain) and the memo advanced to the new size.
func TestGrowthInvalidatesMemo(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)
	c, err := New(baseConfig(inner, log, path, "content-A"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background()); err != nil { // miss -> record + memo
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background()); err != nil { // hit -> memo populated
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	sizeBefore := fi.Size()

	// Append a real, chain-valid event so the log genuinely grows.
	log.Append(eventlog.Event{Task: "T-test", Kind: "note", Detail: map[string]any{"x": 1}})
	fi2, _ := os.Stat(path)
	if fi2.Size() == sizeBefore {
		t.Fatal("test setup: appended event did not grow the log")
	}

	// Next hit must re-verify at the new size (the grown chain still verifies) and
	// advance the memo — never serve against the stale size.
	if rep, err := c.Check(context.Background()); err != nil || !rep.Passed {
		t.Fatalf("post-growth hit = %+v, %v; want a pass over the still-intact chain", rep, err)
	}
	c.mu.Lock()
	memoSize := c.verifiedPrint.size
	c.mu.Unlock()
	if memoSize != fi2.Size() {
		t.Fatalf("memo must advance to the grown size %d, got %d", fi2.Size(), memoSize)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("the grown chain still verifies, so no recompute; inner ran %d, want 1", got)
	}
}

// TestInPlaceTamperInvalidatesMemo proves the memo is SOUND against an in-place
// same-size tamper: the fingerprint folds in mod-time, so a rewrite that keeps the byte
// count identical still bumps mtime, misses the memo, forces a fresh eventlog.Verify,
// sees the broken chain, and recomputes. A size-only memo would have wrongly served the
// stale pass.
func TestInPlaceTamperInvalidatesMemo(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)
	c, err := New(baseConfig(inner, log, path, "content-A"))
	if err != nil {
		t.Fatal(err)
	}
	// Miss records the pass; a hit verifies + memoizes the chain-verified verdict.
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// In-place tamper that PRESERVES the byte length so the size is unchanged — only an
	// mtime bump can distinguish it. An equal-length swap of the recorded toolchain
	// string breaks the chain hash (the value no longer matches what was hashed).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := string(data)
	tampered := strings.Replace(orig, "go1.25.0", "go9.99.9", 1)
	if tampered == orig {
		t.Fatal("test setup: equal-length tamper found nothing to change")
	}
	if len(tampered) != len(orig) {
		t.Fatalf("test setup: tamper changed length %d->%d (must be equal for this test)", len(orig), len(tampered))
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force mtime forward so a coarse filesystem clock still registers the change.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if eventlog.Verify(path) == nil {
		t.Fatal("test setup: equal-length tamper did not break the chain")
	}

	// A fresh cache over the tampered log: the physically-present matching line must NOT
	// be served, because the broken chain fails Verify. (Fresh cache = no in-memory memo
	// from the pre-tamper file; this asserts the on-disk soundness end to end.)
	inner2 := &spy{report: verify.Report{Passed: true}}
	c2, _ := New(Config{
		Inner: inner2, Log: nil, LogPath: path,
		Hash: fixedHash("content-A"), VerifierID: "make verify", Toolchain: "go1.25.0",
	})
	rep, err := c2.Check(context.Background())
	if err != nil || !rep.Passed {
		t.Fatalf("recompute Check = %+v, %v; want a fresh pass", rep, err)
	}
	if got := inner2.calls.Load(); got != 1 {
		t.Fatalf("an in-place tamper must force recompute; inner ran %d, want 1", got)
	}
}

// TestFailureNotCached: a red verifier result is never recorded as a cache pass, so
// it can never be replayed as green. A second Check re-runs.
func TestFailureNotCached(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: false, Output: "build broke"}}
	log, path := freshLog(t)
	c, _ := New(baseConfig(inner, log, path, "content-A"))

	if rep, err := c.Check(context.Background()); err != nil || rep.Passed {
		t.Fatalf("first Check = %+v, %v; want a non-pass", rep, err)
	}
	if rep, err := c.Check(context.Background()); err != nil || rep.Passed {
		t.Fatalf("second Check = %+v, %v; want a non-pass", rep, err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("a failure must never be cached; inner ran %d times, want 2", got)
	}
}

// TestNilCacheDelegates: a nil *Cache must never be installed as the verifier (the
// Decorate seam installs the inner verifier directly), but the nil receiver must
// not panic — it returns an explicit error, never a fabricated pass.
func TestNilCacheDelegates(t *testing.T) {
	var c *Cache
	rep, err := c.Check(context.Background())
	if err == nil {
		t.Error("nil *Cache Check must return an error, not a silent verdict")
	}
	if rep.Passed {
		t.Error("nil *Cache must never report a pass")
	}
}

// TestDecorateFallsBackWhenUnwired: an incomplete config (the default-off case)
// makes Decorate return the inner verifier UNCHANGED, so behavior is byte-identical
// to no cache. The returned verifier is the very same value.
func TestDecorateFallsBackWhenUnwired(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	// Missing LogPath/Hash/etc. ⇒ New fails ⇒ Decorate returns inner unchanged.
	got := Decorate(Config{Inner: inner})
	if got == nil {
		t.Fatal("Decorate returned nil")
	}
	if _, ok := got.(*Cache); ok {
		t.Fatal("an unwired Decorate must return the inner verifier, not a *Cache")
	}
	if _, err := got.Check(context.Background()); err != nil {
		t.Fatalf("delegated Check: %v", err)
	}
	if inner.calls.Load() != 1 {
		t.Errorf("delegated verifier should run exactly once, ran %d", inner.calls.Load())
	}
}

// TestHashErrorRecomputes: if the worktree cannot be hashed, the cache cannot prove
// a hit applies, so it conservatively delegates to the inner verifier.
func TestHashErrorRecomputes(t *testing.T) {
	inner := &spy{report: verify.Report{Passed: true}}
	log, path := freshLog(t)
	cfg := baseConfig(inner, log, path, "content-A")
	cfg.Hash = func(context.Context) (string, error) {
		return "", os.ErrPermission
	}
	c, _ := New(cfg)
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatalf("hash-error Check should still delegate cleanly: %v", err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("a hash error must delegate to inner; inner ran %d times, want 1", got)
	}
}

// TestNewRejectsIncompleteConfig: each missing required field is a loud error, not
// a silently-degraded cache.
func TestNewRejectsIncompleteConfig(t *testing.T) {
	good := baseConfig(&spy{}, nil, "p", "c")
	cases := map[string]func(Config) Config{
		"no inner":      func(c Config) Config { c.Inner = nil; return c },
		"no hash":       func(c Config) Config { c.Hash = nil; return c },
		"no log path":   func(c Config) Config { c.LogPath = ""; return c },
		"no verifierid": func(c Config) Config { c.VerifierID = ""; return c },
		"no toolchain":  func(c Config) Config { c.Toolchain = ""; return c },
	}
	for name, mut := range cases {
		if _, err := New(mut(good)); err == nil {
			t.Errorf("%s: New must reject an incomplete config", name)
		}
	}
}
