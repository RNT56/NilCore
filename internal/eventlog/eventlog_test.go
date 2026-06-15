package eventlog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/store"
)

func TestChainIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		log.Append(Event{Task: "t1", Kind: "step", Detail: map[string]any{"i": i}})
	}
	log.Close()

	if err := Verify(path); err != nil {
		t.Fatalf("Verify a good chain: %v", err)
	}

	// Tamper with the middle event's payload — the chain must catch it.
	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), `"i":1`, `"i":99`, 1)
	if tampered == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(path); err == nil {
		t.Fatal("Verify should detect the tampered event")
	}
}

func TestChainContinuesAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	l1, _ := Open(path)
	l1.Append(Event{Task: "t", Kind: "a"})
	l1.Close()

	l2, _ := Open(path) // must continue the chain, not restart it
	l2.Append(Event{Task: "t", Kind: "b"})
	l2.Close()

	if err := Verify(path); err != nil {
		t.Fatalf("chain across reopen: %v", err)
	}
}

func TestRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, _ := Open(path)
	log.Append(Event{Task: "t", Kind: "tool_exec", Detail: map[string]any{
		"cmd":     "export ANTHROPIC_API_KEY=sk-abc123def456ghi789jkl",
		"api_key": "sk-shouldbegone",
		"note":    "harmless",
	}})
	log.Close()

	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Contains(s, "sk-abc123def456ghi789jkl") {
		t.Error("embedded key not redacted from cmd")
	}
	if strings.Contains(s, "sk-shouldbegone") {
		t.Error("secret-named field not redacted")
	}
	if !strings.Contains(s, "[redacted]") {
		t.Error("expected a redaction marker")
	}
	if !strings.Contains(s, "harmless") {
		t.Error("non-secret content should be preserved")
	}
	// Redaction happens before hashing, so the chain still verifies.
	if err := Verify(path); err != nil {
		t.Fatalf("Verify after redaction: %v", err)
	}
}

func TestStoreBacking(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log.UseStore(s)
	for i := 0; i < 3; i++ {
		log.Append(Event{Task: "t1", Kind: "step", Detail: map[string]any{"i": i}})
	}
	log.Close()

	// Events landed in the store...
	evs, err := s.EventsByTask(context.Background(), "t1")
	if err != nil || len(evs) != 3 {
		t.Fatalf("store events = %d, %v", len(evs), err)
	}
	// ...with the hash chain preserved.
	prev := ""
	for i, e := range evs {
		if e.Prev != prev {
			t.Errorf("event %d: chain break in store", i)
		}
		if e.Hash == "" {
			t.Errorf("event %d: missing hash in store", i)
		}
		prev = e.Hash
	}
	// JSONL export still verifies end to end.
	if err := Verify(path); err != nil {
		t.Errorf("JSONL still must verify: %v", err)
	}
}

// TestStoreErrorSurfaces proves a failed store mirror is surfaced through Err()
// (not silently swallowed) while the authoritative JSONL stays intact and durable.
func TestStoreErrorSurfaces(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log.UseStore(s)
	s.Close() // break the second backing: the next InsertEvent now errors

	log.Append(Event{Task: "t1", Kind: "step"})
	log.Close()

	if log.Err() == nil {
		t.Error("a failed store mirror must surface through Err(), not be swallowed")
	}
	// The authoritative JSONL still holds the event and verifies end to end.
	if err := Verify(path); err != nil {
		t.Errorf("JSONL must stay durable + verifiable despite the store failure: %v", err)
	}
}

// TestAppendKeepsChainConsistentOnWriteFailure proves a failed write neither
// advances the hash chain nor corrupts the log: the failure is surfaced via Err,
// the on-disk chain still verifies, and prev stays anchored to the last durable
// event (audit M4 — previously the error was swallowed and prev advanced anyway).
func TestAppendKeepsChainConsistentOnWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Event{Kind: "first"}) // lands on disk
	anchor := l.prev
	if anchor == "" {
		t.Fatal("expected a chain anchor after the first append")
	}
	if l.Err() != nil {
		t.Fatalf("unexpected early error: %v", l.Err())
	}

	// Force every subsequent write to fail by closing the underlying file.
	if err := l.f.Close(); err != nil {
		t.Fatal(err)
	}
	l.Append(Event{Kind: "second"}) // must fail

	if l.Err() == nil {
		t.Fatal("write failure was swallowed: Err() is nil")
	}
	if l.prev != anchor {
		t.Fatal("hash chain advanced past an event that was never written")
	}
	// The file holds exactly the one good event and still verifies end to end.
	if err := Verify(path); err != nil {
		t.Fatalf("failed append corrupted the log: %v", err)
	}
}

// TestRedactionShapesAndNesting covers the broadened secret shapes (bare AWS key
// ids, GitHub fine-grained PATs, Google API keys, PEM headers — audit L2/L3) and
// recursion into nested maps and slices (audit L5).
func TestRedactionShapesAndNesting(t *testing.T) {
	akia := "AKIAIOSFODNN7EXAMPLE" // bare, no separator (old regex missed it)
	gpat := "github_pat_" + strings.Repeat("A", 24)
	gkey := "AIza" + strings.Repeat("b", 35)
	pem := "-----BEGIN RSA PRIVATE KEY-----"
	sk := "sk-abc123def456ghi789jkl"

	d := map[string]any{
		"aws":    "id=" + akia,
		"gh":     gpat,
		"google": gkey,
		"pem":    pem,
		"nested": map[string]any{
			"cmd":     "run --token " + sk,
			"api_key": "sk-shouldvanish",
		},
		"args": []any{"--secret", sk, "ok"},
		"keep": "harmless",
	}
	redact(d)

	blob, _ := json.Marshal(d)
	s := string(blob)
	for _, leak := range []string{akia, gpat, gkey, pem, sk, "sk-shouldvanish"} {
		if strings.Contains(s, leak) {
			t.Errorf("secret leaked through redaction: %q present in %s", leak, s)
		}
	}
	if !strings.Contains(s, "harmless") {
		t.Error("non-secret content should be preserved")
	}
}

// TestHMACKeyedChain proves a keyed chain verifies under its key but not without
// it or under a different key — so an attacker who cannot read NILCORE_LOG_HMAC_KEY
// cannot forge a chain that passes Verify (audit L6).
func TestHMACKeyedChain(t *testing.T) {
	t.Setenv("NILCORE_LOG_HMAC_KEY", "k-super-secret")
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		log.Append(Event{Task: "t", Kind: "step", Detail: map[string]any{"i": i}})
	}
	log.Close()

	if err := Verify(path); err != nil {
		t.Fatalf("keyed chain should verify with its key: %v", err)
	}
	t.Setenv("NILCORE_LOG_HMAC_KEY", "")
	if err := Verify(path); err == nil {
		t.Fatal("keyed chain verified with no key — HMAC not enforced")
	}
	t.Setenv("NILCORE_LOG_HMAC_KEY", "wrong-key")
	if err := Verify(path); err == nil {
		t.Fatal("keyed chain verified under the wrong key")
	}
}

// TestSequenceAnchorDetectsReorder proves the per-event sequence number catches a
// reordering of otherwise-valid lines (audit L6).
func TestSequenceAnchorDetectsReorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, _ := Open(path)
	log.Append(Event{Kind: "a"})
	log.Append(Event{Kind: "b"})
	log.Append(Event{Kind: "c"})
	log.Close()

	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[1], lines[2] = lines[2], lines[1] // swap two events
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(path); err == nil {
		t.Fatal("reordered events were not detected")
	}
}
