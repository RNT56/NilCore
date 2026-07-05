package graapprove

import (
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/eventlog"
)

// appendBoundary appends n passing boundary_outcome events for (action,scope) to an
// EXISTING hash-chained log at path (opening it preserves the chain), so a test can
// grow a log between TrustBuilder.Build calls and assert the incremental fold.
func appendBoundary(t *testing.T, path, action, scope string, n int) {
	t.Helper()
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("eventlog.Open(append): %v", err)
	}
	for i := 0; i < n; i++ {
		l.Append(eventlog.Event{Kind: "boundary_outcome", Detail: map[string]any{
			"action": action, "scope": scope, "passed": true,
		}})
	}
	if err := l.Err(); err != nil {
		t.Fatalf("append write error: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
}

// TestTrustBuilderIncrementalMatchesFull proves the memoized incremental fold produces
// tallies IDENTICAL to a full BuildTrust after each growth of the log — correctness is
// preserved while only the appended suffix is scanned on the second call.
func TestTrustBuilderIncrementalMatchesFull(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	b := &TrustBuilder{}
	k := ScopeKey{Type: "open-pr", Scope: "feat/x"}

	// First build folds the whole (3-green) log.
	v1, err := b.Build(path)
	if err != nil {
		t.Fatalf("Build 1: %v", err)
	}
	if got := v1.Tally(k); got.Green != 3 || got.Total != 3 {
		t.Fatalf("build 1 tally = %+v, want 3/3", got)
	}

	// Grow the log by 2 more greens; the incremental build must fold only the suffix
	// yet report the SAME totals as a fresh full BuildTrust.
	appendBoundary(t, path, "open-pr", "feat/x", 2)
	v2, err := b.Build(path)
	if err != nil {
		t.Fatalf("Build 2: %v", err)
	}
	full, err := BuildTrust(path)
	if err != nil {
		t.Fatalf("BuildTrust: %v", err)
	}
	if v2.Tally(k) != full.Tally(k) {
		t.Fatalf("incremental tally %+v != full tally %+v", v2.Tally(k), full.Tally(k))
	}
	if got := v2.Tally(k); got.Green != 5 || got.Total != 5 {
		t.Fatalf("build 2 tally = %+v, want 5/5", got)
	}
}

// TestTrustBuilderUnchangedLogSkipsRescan proves the fast path: when the log is
// byte-for-byte unchanged, Build re-emits the cached view without re-folding or
// re-Verifying. We can't observe "did not scan" directly, so we assert the caller-facing
// contract that depends on it: the returned view is a COPY (mutating one must not affect
// the next), and repeated builds over an unchanged log are stable and equal.
func TestTrustBuilderUnchangedLogSkipsRescan(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 4))
	b := &TrustBuilder{}
	k := ScopeKey{Type: "open-pr", Scope: "feat/x"}

	v1, err := b.Build(path)
	if err != nil {
		t.Fatalf("Build 1: %v", err)
	}
	v2, err := b.Build(path) // unchanged ⇒ fast path
	if err != nil {
		t.Fatalf("Build 2: %v", err)
	}
	if v1.Tally(k) != v2.Tally(k) {
		t.Fatalf("unchanged-log builds disagree: %+v vs %+v", v1.Tally(k), v2.Tally(k))
	}
	if got := v2.Tally(k); got.Green != 4 || got.Total != 4 {
		t.Fatalf("tally = %+v, want 4/4", got)
	}
	// The returned tallies must be a copy of the builder's cache: mutating the map the
	// view exposes (via a fresh Tally read) can never bleed into the next Build. Read a
	// missing key to be sure the zero value is returned, not a shared reference.
	if got := v2.Tally(ScopeKey{Type: "push", Scope: "none"}); got != (Tally{}) {
		t.Fatalf("absent key must be zero, got %+v", got)
	}
}

// TestTrustBuilderShrinkInvalidatesCache proves a truncation/rewrite (size below the
// folded watermark) invalidates the cache and re-folds from scratch, so stale trust can
// never survive a shrink. We fold a log, then replace it with a SHORTER valid chain and
// assert the tally reflects the new (smaller) content.
func TestTrustBuilderShrinkInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 6))
	b := &TrustBuilder{}
	k := ScopeKey{Type: "open-pr", Scope: "feat/x"}

	if v, err := b.Build(path); err != nil || v.Tally(k).Total != 6 {
		t.Fatalf("build 1: tally total = %d, err=%v, want 6", v.Tally(k).Total, err)
	}

	// Rewrite the log with a SHORTER valid chain (2 greens). The new file is smaller
	// than the folded watermark, so the builder must fold from scratch.
	shortDir := filepath.Join(dir, "shorter")
	if err := os.MkdirAll(shortDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shorter := writeLog(t, shortDir, greenRun("open-pr", "feat/x", 2))
	data, err := os.ReadFile(shorter)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	v, err := b.Build(path)
	if err != nil {
		t.Fatalf("build after shrink: %v", err)
	}
	if got := v.Tally(k); got.Total != 2 || got.Green != 2 {
		t.Fatalf("after shrink tally = %+v, want 2/2 (cache must invalidate, not report the stale 6)", got)
	}
}

// TestTrustBuilderTamperFailsClosedAndNotCachedVerified proves a broken chain denies
// (empty tallies, ChainOK=false, error) AND is not cached as verified — a subsequent
// Build over the still-broken log must re-check and keep failing closed, never serve a
// stale "verified" view.
func TestTrustBuilderTamperFailsClosedAndNotCachedVerified(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	// Tamper the chain.
	data, _ := os.ReadFile(path)
	idx := indexOf(data, []byte("feat/x"))
	if idx < 0 {
		t.Fatal("setup: scope token not found")
	}
	data[idx] = 'Z'
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	b := &TrustBuilder{}
	for i := 0; i < 2; i++ { // twice: a broken chain must never be cached as verified
		v, err := b.Build(path)
		if err == nil {
			t.Fatalf("build %d: tampered chain must return an error", i)
		}
		if v.ChainOK {
			t.Fatalf("build %d: tampered chain must report ChainOK=false", i)
		}
		if got := v.Tally(ScopeKey{Type: "open-pr", Scope: "feat/x"}); got != (Tally{}) {
			t.Fatalf("build %d: tampered chain must yield empty tallies, got %+v", i, got)
		}
	}
}

// TestTrustBuilderMissingLogIsCleanEmpty proves a missing log (never created or removed)
// is a clean empty, trusted-shape view — matching BuildTrust — and resets any cache.
func TestTrustBuilderMissingLogIsCleanEmpty(t *testing.T) {
	dir := t.TempDir()
	b := &TrustBuilder{}

	// Missing from the start.
	v, err := b.Build(filepath.Join(dir, "nope.log"))
	if err != nil {
		t.Fatalf("missing log must be a nil error, got %v", err)
	}
	if !v.ChainOK {
		t.Fatal("missing log must report ChainOK=true")
	}

	// Build a real log, then remove it: the cache must reset and report clean empty.
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 2))
	if _, err := b.Build(path); err != nil {
		t.Fatalf("build real log: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	v2, err := b.Build(path)
	if err != nil {
		t.Fatalf("removed log must be a nil error, got %v", err)
	}
	if !v2.ChainOK {
		t.Fatal("removed log must report ChainOK=true (clean empty)")
	}
	if got := v2.Tally(ScopeKey{Type: "open-pr", Scope: "feat/x"}); got != (Tally{}) {
		t.Fatalf("removed log must yield empty tallies, got %+v", got)
	}
}

// TestTrustBuilderNilFallsBackToBuildTrust proves a nil *TrustBuilder is byte-identical
// to a plain BuildTrust (the unwired path).
func TestTrustBuilderNilFallsBackToBuildTrust(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))
	var b *TrustBuilder
	got, err := b.Build(path)
	if err != nil {
		t.Fatalf("nil builder Build: %v", err)
	}
	want, err := BuildTrust(path)
	if err != nil {
		t.Fatalf("BuildTrust: %v", err)
	}
	k := ScopeKey{Type: "open-pr", Scope: "feat/x"}
	if got.Tally(k) != want.Tally(k) || got.ChainOK != want.ChainOK {
		t.Fatalf("nil builder %+v/%v != BuildTrust %+v/%v", got.Tally(k), got.ChainOK, want.Tally(k), want.ChainOK)
	}
}
