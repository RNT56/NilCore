package trust

import (
	"os"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// classRaceEvt is a race_outcome carrying the Phase-16 routing dimensions:
// Detail{"passed", "class", "cost"} — the exact shape the router writes.
func classRaceEvt(task, be, class string, passed bool, cost float64) eventlog.Event {
	return eventlog.Event{
		Task: task, Backend: be, Kind: "race_outcome",
		Detail: map[string]any{"passed": passed, "class": class, "cost": cost},
	}
}

// TestReplayFoldsClassAndCost: a log of class-tagged race outcomes round-trips
// through Replay into the per-(class, backend) cells with cost accumulated, while
// the global scoreboard sums across classes. JSON decodes cost as float64, which
// is exactly what replay.go reads.
func TestReplayFoldsClassAndCost(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		classRaceEvt("t1", "native", "refactor", true, 0.10),
		classRaceEvt("t1", "codex", "refactor", false, 0.20),
		classRaceEvt("t2", "native", "bugfix", true, 0.30),
		classRaceEvt("t2", "native", "bugfix", false, 0.05),
	})

	l, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay class log: %v", err)
	}

	ref := l.ClassStandings("refactor")
	gotRef := map[string]ClassStat{}
	for _, c := range ref {
		gotRef[c.Backend] = c
	}
	if c := gotRef["native"]; c.Races != 1 || c.Wins != 1 || c.TotalCost != 0.10 {
		t.Errorf("refactor native = %+v, want races=1 wins=1 cost=0.10", c)
	}
	if c := gotRef["codex"]; c.Races != 1 || c.Wins != 0 || c.TotalCost != 0.20 {
		t.Errorf("refactor codex = %+v, want races=1 wins=0 cost=0.20", c)
	}

	bug := l.ClassStandings("bugfix")
	if len(bug) != 1 {
		t.Fatalf("bugfix standings = %+v, want 1 cell", bug)
	}
	if c := bug[0]; c.Backend != "native" || c.Races != 2 || c.Wins != 1 || c.TotalCost != 0.35 {
		t.Errorf("bugfix native = %+v, want native races=2 wins=1 cost=0.35", c)
	}

	// Global scoreboard sums both classes: native 3 races / 2 wins, codex 1/0.
	got := map[string]Stat{}
	for _, s := range l.Snapshot().Backends {
		got[s.Backend] = s
	}
	if n := got["native"]; n.Races != 3 || n.Wins != 2 {
		t.Errorf("global native = %+v, want races=3 wins=2", n)
	}
	if c := got["codex"]; c.Races != 1 || c.Wins != 0 {
		t.Errorf("global codex = %+v, want races=1 wins=0", c)
	}
}

// TestReplayNoClassIsByteIdentical is the backward-compat guarantee: a
// pre-Phase-16 log (race_outcome events with NO class and NO cost in Detail)
// replays to the exact same ledger as one built by Record without a class. The
// global scoreboard is unchanged, the cells all live under the "" class, and no
// cost is fabricated.
func TestReplayNoClassIsByteIdentical(t *testing.T) {
	// A log in the OLD shape — raceEvt writes Detail{"passed"} only, no class/cost.
	path := buildLog(t, []eventlog.Event{
		raceEvt("t1", "native", true),
		raceEvt("t1", "codex", false),
		raceEvt("t2", "native", true),
		raceEvt("t2", "codex", true),
	})
	replayed, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay no-class log: %v", err)
	}

	// The reference: the same outcomes folded by hand, Class unset.
	ref := New()
	ref.Record(Outcome{Backend: "native", Passed: true})
	ref.Record(Outcome{Backend: "codex", Passed: false})
	ref.Record(Outcome{Backend: "native", Passed: true})
	ref.Record(Outcome{Backend: "codex", Passed: true})

	gotSnap, wantSnap := replayed.Snapshot(), ref.Snapshot()
	if !snapshotsEqual(gotSnap, wantSnap) {
		t.Fatalf("no-class replay diverged from a Class-less fold:\n got %+v\nwant %+v", gotSnap, wantSnap)
	}

	// Every replayed cell must live under the "" class, and carry zero cost.
	for _, c := range gotSnap.Classes {
		if c.Class != "" {
			t.Errorf("no-class replay produced a non-empty class cell: %+v", c)
		}
		if c.TotalCost != 0 {
			t.Errorf("no-class replay fabricated cost: %+v", c)
		}
	}
}

// snapshotsEqual compares two snapshots field-by-field; the slices are already in
// deterministic order so a direct comparison is meaningful.
func snapshotsEqual(a, b Snapshot) bool {
	if len(a.Backends) != len(b.Backends) || len(a.Configs) != len(b.Configs) || len(a.Classes) != len(b.Classes) {
		return false
	}
	for i := range a.Backends {
		if a.Backends[i] != b.Backends[i] {
			return false
		}
	}
	for i := range a.Configs {
		if a.Configs[i] != b.Configs[i] {
			return false
		}
	}
	for i := range a.Classes {
		if a.Classes[i] != b.Classes[i] {
			return false
		}
	}
	return true
}

// TestReplayClassLogTamperedFailsClosed: a class-tagged log whose hash chain is
// broken must still fail closed — Replay returns an error and a nil ledger. Reuses
// the existing tamper pattern (flip a recorded value, the chain catches it).
func TestReplayClassLogTamperedFailsClosed(t *testing.T) {
	path := buildLog(t, []eventlog.Event{
		classRaceEvt("t1", "native", "refactor", true, 0.10),
		classRaceEvt("t1", "codex", "refactor", false, 0.20),
		classRaceEvt("t1", "claude", "bugfix", true, 0.30),
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Forge a class label in a recorded event; the hash chain must reject it.
	tampered := strings.Replace(string(data), `"refactor"`, `"forged"`, 1)
	if tampered == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Replay(path)
	if err == nil {
		t.Fatal("Replay must error on a tampered class log")
	}
	if l != nil {
		t.Errorf("Replay must return a nil ledger on a tampered chain, got %+v", l)
	}
}
