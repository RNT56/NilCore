package trust

import (
	"os"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// finalVerifyEvt is the exact shape the orchestrator writes since the Phase-16
// upgrade: Kind "final_verify", the backend on the event's top-level field, and
// Detail{"passed", "class"} (plus "fail_class" on a failure, which Replay ignores).
func finalVerifyEvt(task, be, class string, passed bool) eventlog.Event {
	return eventlog.Event{
		Task: task, Backend: be, Kind: "final_verify",
		Detail: map[string]any{"passed": passed, "class": class},
	}
}

// TestReplayFoldsFinalVerify: class-tagged final_verify events fold into BOTH the
// global per-backend scoreboard and the per-(class, backend) cells — the signal
// that warms trust on a single-backend deployment where no race ever fires — and
// they fold ALONGSIDE race_outcome events (different attempts, no double count).
func TestReplayFoldsFinalVerify(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		finalVerifyEvt("t1", "native", "bugfix", true),
		finalVerifyEvt("t2", "native", "bugfix", false),
		finalVerifyEvt("t3", "native", "refactor", true),
		// t4 raced after its final_verify failed: ONE final_verify (the first
		// attempt) plus per-candidate race_outcomes (fresh attempts) — each event is
		// a distinct verifier judgement, so all three fold.
		finalVerifyEvt("t4", "native", "bugfix", false),
		raceEvt("t4", "native", false),
		raceEvt("t4", "codex", true),
	})

	l, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay final_verify log: %v", err)
	}

	// Per-class cells: bugfix native = 3 attempts (t1 pass, t2 fail, t4 fail) — the
	// race_outcome events carry no class, so they land in the "" cell, not here.
	bug := l.ClassStandings("bugfix")
	if len(bug) != 1 {
		t.Fatalf("bugfix standings = %+v, want 1 cell", bug)
	}
	if c := bug[0]; c.Backend != "native" || c.Races != 3 || c.Wins != 1 {
		t.Errorf("bugfix native = %+v, want races=3 wins=1", c)
	}
	ref := l.ClassStandings("refactor")
	if len(ref) != 1 || ref[0].Backend != "native" || ref[0].Races != 1 || ref[0].Wins != 1 {
		t.Errorf("refactor standings = %+v, want native races=1 wins=1", ref)
	}

	// Global scoreboard sums final_verify AND race_outcome:
	// native = 4 final_verify (2 wins) + 1 race_outcome (0 wins) = 5 races / 2 wins;
	// codex = 1 race_outcome / 1 win.
	got := map[string]Stat{}
	for _, s := range l.Snapshot().Backends {
		got[s.Backend] = s
	}
	if n := got["native"]; n.Races != 5 || n.Wins != 2 {
		t.Errorf("global native = %+v, want races=5 wins=2", n)
	}
	if c := got["codex"]; c.Races != 1 || c.Wins != 1 {
		t.Errorf("global codex = %+v, want races=1 wins=1", c)
	}
}

// TestReplayMalformedFinalVerifySkipped: a final_verify missing its class, its
// backend, or carrying a garbage verdict is SKIPPED — never folded, never a
// panic. Untrusted Detail fields are guarded before use (the forged-count
// lesson), and a defaulted-false verdict would fabricate a loss, so anything
// short of a real JSON bool drops the event.
func TestReplayMalformedFinalVerifySkipped(t *testing.T) {
	cases := []struct {
		name string
		evt  eventlog.Event
	}{
		{"missing class", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify",
			Detail: map[string]any{"passed": true}}},
		{"empty class", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify",
			Detail: map[string]any{"passed": true, "class": ""}}},
		{"non-string class", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify",
			Detail: map[string]any{"passed": true, "class": 42}}},
		{"missing backend", eventlog.Event{Task: "t", Kind: "final_verify",
			Detail: map[string]any{"passed": true, "class": "bugfix"}}},
		{"missing passed", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify",
			Detail: map[string]any{"class": "bugfix"}}},
		{"garbage passed string", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify",
			Detail: map[string]any{"passed": "yes", "class": "bugfix"}}},
		{"garbage passed number", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify",
			Detail: map[string]any{"passed": 1, "class": "bugfix"}}},
		{"nil detail", eventlog.Event{Task: "t", Backend: "native", Kind: "final_verify"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A well-formed sibling proves the malformed event is skipped, not fatal.
			path := buildLog(t, []eventlog.Event{
				tc.evt,
				finalVerifyEvt("ok", "codex", "docs", true),
			})
			l, err := Replay(path) // must not panic
			if err != nil {
				t.Fatalf("Replay errored on a malformed final_verify: %v", err)
			}
			snap := l.Snapshot()
			if len(snap.Backends) != 1 || snap.Backends[0].Backend != "codex" {
				t.Errorf("malformed final_verify must fold nothing; Backends=%+v", snap.Backends)
			}
			if cells := l.ClassStandings("bugfix"); cells != nil {
				t.Errorf("malformed final_verify leaked a class cell: %+v", cells)
			}
		})
	}
}

// TestReplayFinalVerifyTamperedFailsClosed: a chain-tampered log containing
// final_verify events still refuses to fold — Replay returns an error and a nil
// ledger, same fail-closed discipline as the race_outcome fold.
func TestReplayFinalVerifyTamperedFailsClosed(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		finalVerifyEvt("t1", "native", "bugfix", true),
		finalVerifyEvt("t2", "codex", "bugfix", false),
	})

	// Forge the recorded verdict; the hash chain must catch it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"passed":false`, `"passed":true`, 1)
	if tampered == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Replay(path)
	if err == nil {
		t.Fatal("Replay must error on a tampered final_verify log")
	}
	if l != nil {
		t.Errorf("Replay must return a nil ledger on a tampered chain, got %+v", l)
	}
}
