package distiller_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/flywheel/distiller"
)

// writeLog builds a real hash-chained event log via eventlog.Open/Append (the
// production write path) and returns its path. Every test feeds the distiller a
// genuinely-chained log so the eventlog.Verify fail-closed behaviour is exercised
// against real hashes, not a hand-rolled file.
func writeLog(t *testing.T, events []eventlog.Event) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, e := range events {
		l.Append(e)
	}
	if err := l.Err(); err != nil {
		t.Fatalf("log write failed: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return path
}

// fail/pass are tiny constructors for a verifier verdict event with the LRN-T01
// structural enrichment keys, plus a RAW OUTPUT channel the distiller must never
// copy into a Pattern (I7).
func fail(verifierID, failClass, backend, rawOutput string) eventlog.Event {
	return eventlog.Event{
		Kind:    "final_verify",
		Backend: backend,
		Detail: map[string]any{
			"passed":      false,
			"verifier_id": verifierID,
			"fail_class":  failClass,
			"output":      rawOutput, // attacker-influenced text; must NOT leak
		},
	}
}

func pass(verifierID, failClass, backend string) eventlog.Event {
	return eventlog.Event{
		Kind:    "final_verify",
		Backend: backend,
		Detail: map[string]any{
			"passed":      true,
			"verifier_id": verifierID,
			"fail_class":  failClass,
		},
	}
}

func TestRecurringFailuresClusterWithCounts(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		fail("go-test", "test", "native", "panic: nil deref at frobnicate.go:42"),
		fail("go-test", "test", "native", "panic: nil deref at frobnicate.go:7"),
		fail("go-test", "test", "native", "FAIL secret=hunter2 in output"),
		fail("go-vet", "lint", "codex", "vet: shadowed err"),
		fail("go-vet", "lint", "codex", "vet: unreachable code"),
		pass("go-test", "test", "native"), // a later green for the same coordinate
	})

	got, err := distiller.Distill(path, 0) // 0 ⇒ DefaultThreshold (2)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 clustered patterns, got %d: %+v", len(got), got)
	}

	// Strongest recurrence first (3 > 2).
	if got[0].VerifierID != "go-test" || got[0].Count != 3 {
		t.Errorf("pattern[0] = %+v, want VerifierID=go-test Count=3", got[0])
	}
	// go-test had 3 fails + 1 pass = 4 verdicts observed.
	if got[0].Sample != 4 {
		t.Errorf("pattern[0].Sample = %d, want 4", got[0].Sample)
	}
	if got[1].VerifierID != "go-vet" || got[1].Count != 2 {
		t.Errorf("pattern[1] = %+v, want VerifierID=go-vet Count=2", got[1])
	}
	if got[0].Kind != distiller.Kind {
		t.Errorf("pattern Kind = %q, want %q", got[0].Kind, distiller.Kind)
	}
	if got[0].FailClass != "test" || got[1].FailClass != "lint" {
		t.Errorf("fail classes not preserved: %q, %q", got[0].FailClass, got[1].FailClass)
	}
	if fr := got[0].FailRate(); fr != 0.75 {
		t.Errorf("go-test FailRate = %v, want 0.75", fr)
	}
}

func TestSingleFailuresFiltered(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		fail("go-test", "test", "native", "transient flake"),   // one-off
		fail("go-build", "build", "native", "transient flake"), // one-off
		fail("go-vet", "lint", "native", "real scar 1"),
		fail("go-vet", "lint", "native", "real scar 2"),
	})

	got, err := distiller.Distill(path, 0)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want only the recurring pattern, got %d: %+v", len(got), got)
	}
	if got[0].VerifierID != "go-vet" || got[0].Count != 2 {
		t.Errorf("got %+v, want the go-vet x2 cluster", got[0])
	}
}

func TestThresholdRaised(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		fail("a", "test", "native", "x"),
		fail("a", "test", "native", "x"),
		fail("b", "test", "native", "x"),
		fail("b", "test", "native", "x"),
		fail("b", "test", "native", "x"),
	})

	// threshold=3 keeps only b (3 fails), drops a (2 fails).
	got, err := distiller.Distill(path, 3)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(got) != 1 || got[0].VerifierID != "b" || got[0].Count != 3 {
		t.Fatalf("threshold=3 should keep only b x3, got %+v", got)
	}
}

func TestNoRawOutputInPattern(t *testing.T) {
	const secret = "secret=hunter2 panic: nil deref at frobnicate.go:42"
	path := writeLog(t, []eventlog.Event{
		fail("go-test", "test", "native", secret),
		fail("go-test", "test", "native", secret),
	})

	got, err := distiller.Distill(path, 0)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 pattern, got %d", len(got))
	}

	// I7: no field of a Pattern may carry the raw, attacker-influenced output.
	// Walk every string field of every pattern and assert the marker is absent.
	for _, p := range got {
		v := reflect.ValueOf(p)
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.Kind() != reflect.String {
				continue
			}
			if strings.Contains(f.String(), "hunter2") || strings.Contains(f.String(), "panic") || strings.Contains(f.String(), "frobnicate") {
				t.Errorf("Pattern field %q leaked raw output: %q",
					v.Type().Field(i).Name, f.String())
			}
		}
	}
}

func TestTamperFailsClosed(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		fail("go-test", "test", "native", "x"),
		fail("go-test", "test", "native", "x"),
	})

	// Tamper: flip a byte in the middle of the chained file so a hash no longer
	// links. The distiller must drop the patterns it folded and surface the error.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	// Replace the first "false" verdict with "truee" → same length is not needed;
	// any content edit breaks the hash. Use a targeted replace that keeps valid
	// JSON line structure so the failure comes from Verify, not a parse error.
	tampered := strings.Replace(string(data), `"go-test"`, `"go-tesT"`, 1)
	if tampered == string(data) {
		t.Fatal("tamper did not change the file")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}

	got, err := distiller.Distill(path, 0)
	if err == nil {
		t.Fatal("expected a fail-closed error over a tampered chain, got nil")
	}
	if got != nil {
		t.Errorf("tampered log must yield NO patterns, got %+v", got)
	}
}

func TestMissingLogIsCleanEmpty(t *testing.T) {
	got, err := distiller.Distill(filepath.Join(t.TempDir(), "nope.jsonl"), 0)
	if err != nil {
		t.Fatalf("missing log should be a clean empty result, got error: %v", err)
	}
	if got != nil {
		t.Errorf("missing log should yield no patterns, got %+v", got)
	}
}

func TestEmptyLogIsCleanEmpty(t *testing.T) {
	path := writeLog(t, nil) // opens + closes, writes nothing
	got, err := distiller.Distill(path, 0)
	if err != nil {
		t.Fatalf("empty log: %v", err)
	}
	// A successfully-replayed log that folds nothing yields an empty result (a
	// non-nil zero-length slice is fine — what matters is "no improvement targets").
	if len(got) != 0 {
		t.Errorf("empty log should yield no patterns, got %+v", got)
	}
}

// TestOnlyVerifierVerdictsFold proves I2: a non-verify event and a verify event
// with no explicit Detail["passed"] never contribute a scar, and only an EXPLICIT
// false counts — a backend self-claim (here, a task_run with passed:false in its
// own Detail, which is NOT a verify-family kind) is ignored.
func TestOnlyVerifierVerdictsFold(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		// Self-report-shaped, but a non-verify kind ⇒ never folded.
		{Kind: "task_run", Backend: "native", Detail: map[string]any{"passed": false}},
		{Kind: "task_run", Backend: "native", Detail: map[string]any{"passed": false}},
		// Verify event with no verdict ⇒ never folded (absent evidence is no scar).
		{Kind: "final_verify", Backend: "native", Detail: map[string]any{"verifier_id": "v"}},
		{Kind: "final_verify", Backend: "native", Detail: map[string]any{"verifier_id": "v"}},
		// Two genuine verifier failures ⇒ the only thing that folds.
		fail("real", "test", "native", "x"),
		fail("real", "test", "native", "x"),
	})

	got, err := distiller.Distill(path, 0)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(got) != 1 || got[0].VerifierID != "real" || got[0].Count != 2 {
		t.Fatalf("only verifier verdicts must fold, got %+v", got)
	}
}

// TestUnenrichedLogStillClusters proves the miner works on a log that predates
// LRN-T01 enrichment: with no verifier_id/fail_class, failures still cluster by
// their structural (kind, backend) coordinate, with an "unknown" fail class.
func TestUnenrichedLogStillClusters(t *testing.T) {
	bare := func() eventlog.Event {
		return eventlog.Event{Kind: "verify", Backend: "native", Detail: map[string]any{"passed": false}}
	}
	path := writeLog(t, []eventlog.Event{bare(), bare(), bare()})

	got, err := distiller.Distill(path, 0)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 cluster from the bare log, got %d: %+v", len(got), got)
	}
	if got[0].VerifierID != "verify" {
		t.Errorf("bare log should key VerifierID on the event kind, got %q", got[0].VerifierID)
	}
	if got[0].FailClass != "unknown" {
		t.Errorf("bare log should bucket fail class as unknown, got %q", got[0].FailClass)
	}
	if got[0].Count != 3 {
		t.Errorf("count = %d, want 3", got[0].Count)
	}
}
