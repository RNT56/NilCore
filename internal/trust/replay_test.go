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
		// A PRE-upgrade final_verify (no backend, no class) is skipped — only the
		// class-tagged, backend-attributed shape folds (see replay_finalverify_test.go).
		{Task: "t1", Kind: "final_verify", Detail: map[string]any{"passed": true}},
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

// TestReplayFoldsSelfevalReports: a selfeval_report event (emitted by flywheel
// selfeval.Fold) folds into the per-config EVIDENCE view, NOT the routing standings —
// so a self-eval pass-rate informs the operator without steering backend choice.
func TestReplayFoldsSelfevalReports(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		raceEvt("t1", "native", true), // a normal routing outcome
		{Kind: "selfeval_report", Detail: map[string]any{
			"config": "flywheel", "cases": 10, "passes": 8, "pass_rate": 0.8, "chain_ok": true,
		}},
		{Kind: "selfeval_report", Detail: map[string]any{ // empty config ⇒ ignored
			"config": "", "cases": 3, "pass_rate": 1.0,
		}},
	})

	l, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	snap := l.Snapshot()

	// The self-eval report lands in the config evidence view with its pass-rate/cases.
	var found *ConfigStat
	for i := range snap.Configs {
		if snap.Configs[i].Config == "flywheel" {
			found = &snap.Configs[i]
		}
	}
	if found == nil {
		t.Fatalf("selfeval_report did not fold into Configs: %+v", snap.Configs)
	}
	if found.PassRate != 0.8 || found.Cases != 10 {
		t.Errorf("folded config = %+v, want pass_rate=0.8 cases=10", *found)
	}
	if len(snap.Configs) != 1 {
		t.Errorf("empty-config selfeval_report must fold nothing; Configs=%+v", snap.Configs)
	}
	// And it MUST NOT have created a routing standing (configs ≠ backend scoreboard).
	if len(snap.Backends) != 1 || snap.Backends[0].Backend != "native" {
		t.Errorf("selfeval folds must not touch routing standings; Backends=%+v", snap.Backends)
	}
}

// TestReplayForgedSelfevalCount: a selfeval_report with a negative or absurdly large
// `cases` count must be SKIPPED, never allocated from — `make([]Result, int(casesF))`
// would otherwise panic ("makeslice: len out of range") and crash trust.Replay (the
// routing hot path) BEFORE the chain check could reject a tampered log.
func TestReplayForgedSelfevalCount(t *testing.T) {
	for _, cases := range []any{-5, 999999999, 1e18} {
		path := buildLog(t, []eventlog.Event{
			raceEvt("t1", "native", true),
			{Kind: "selfeval_report", Detail: map[string]any{"config": "x", "pass_rate": 1.0, "cases": cases}},
		})
		l, err := Replay(path) // must not panic
		if err != nil {
			t.Fatalf("cases=%v: Replay errored: %v", cases, err)
		}
		// The forged-count event folds nothing; the race outcome still folds normally.
		if got := len(l.Snapshot().Configs); got != 0 {
			t.Errorf("cases=%v: a forged selfeval count must fold nothing, got %d configs", cases, got)
		}
		if got := len(l.Snapshot().Backends); got != 1 {
			t.Errorf("cases=%v: the race outcome must still fold, got %d backends", cases, got)
		}
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
