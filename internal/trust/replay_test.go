package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// buildLog appends the given events to a fresh hash-chained log and returns its
// path. Mirrors the inspect package's test helper so the chain is real (Replay
// runs the genuine eventlog.Verify over it).
func buildLog(t *testing.T, events []eventlog.Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, e := range events {
		log.Append(e)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return path
}

// raceEvt is the exact shape route.Race writes: Kind "race_outcome", a backend,
// and Detail{"passed": bool}.
func raceEvt(task, be string, passed bool) eventlog.Event {
	return eventlog.Event{
		Task: task, Backend: be, Kind: "race_outcome",
		Detail: map[string]any{"passed": passed},
	}
}

// TestReplayFoldsRaceOutcomes: only race_outcome events fold in, the verdict comes
// from Detail["passed"], and unrelated kinds are ignored.
func TestReplayFoldsRaceOutcomes(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		{Task: "t1", Kind: "task_start"}, // ignored: not a race
		raceEvt("t1", "native", true),
		raceEvt("t1", "codex", false),
		{Task: "t1", Kind: "final_verify", Detail: map[string]any{"passed": true}}, // ignored: wrong kind
		raceEvt("t2", "native", true),
		raceEvt("t2", "codex", true),
	})

	l, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay good log: %v", err)
	}
	snap := l.Snapshot()

	got := map[string]Stat{}
	for _, s := range snap.Backends {
		got[s.Backend] = s
	}
	if n := got["native"]; n.Races != 2 || n.Wins != 2 {
		t.Errorf("native = %+v, want races=2 wins=2", n)
	}
	if c := got["codex"]; c.Races != 2 || c.Wins != 1 {
		t.Errorf("codex = %+v, want races=2 wins=1", c)
	}
	// The non-race events must not have leaked a row.
	if len(got) != 2 {
		t.Errorf("snapshot has %d backends, want 2 (only race_outcome folds): %v", len(got), got)
	}
}

// TestReplayTamperedLogFailsClosed: corrupt one line and assert Replay returns an
// error and a nil ledger — no ranking is ever earned over a broken chain.
func TestReplayTamperedLogFailsClosed(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		raceEvt("t1", "native", true),
		raceEvt("t1", "codex", false),
		raceEvt("t1", "claude", true),
	})

	// Flip a recorded verdict in the middle event. The hash chain must catch it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"codex"`, `"forged"`, 1)
	if tampered == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Replay(path)
	if err == nil {
		t.Fatal("Replay must error on a tampered chain")
	}
	if l != nil {
		t.Errorf("Replay must return a nil ledger on a tampered chain, got %+v", l)
	}
}

// TestReplayMissingLogIsEmptyLedger: a missing log is a clean empty ledger, not an
// error — a fresh install simply has no earned signal yet.
func TestReplayMissingLogIsEmptyLedger(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	l, err := Replay(missing)
	if err != nil {
		t.Fatalf("Replay of a missing log must be clean, got: %v", err)
	}
	if l == nil {
		t.Fatal("Replay of a missing log must return an empty ledger, got nil")
	}
	if got := len(l.Snapshot().Backends); got != 0 {
		t.Errorf("missing-log ledger has %d backends, want 0", got)
	}
	if got := l.Rank(); got != nil {
		t.Errorf("missing-log ledger Rank = %v, want nil", got)
	}
}

// TestReplayEmptyLog: an empty (zero-event) log is readable, verifies trivially,
// and yields an empty ledger.
func TestReplayEmptyLog(t *testing.T) {
	path := buildLog(t, nil)
	l, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay empty log: %v", err)
	}
	if got := len(l.Snapshot().Backends); got != 0 {
		t.Errorf("empty-log ledger has %d backends, want 0", got)
	}
}

// TestReplayCorruptJSONLine: a non-JSON line is a parse failure surfaced as an
// error, never a silently-dropped event.
func TestReplayCorruptJSONLine(t *testing.T) {
	path := buildLog(t, []eventlog.Event{raceEvt("t1", "native", true)})
	if err := os.WriteFile(path, []byte("{not valid json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(path); err == nil {
		t.Error("Replay must error on an unparseable line")
	}
}
