package vcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
