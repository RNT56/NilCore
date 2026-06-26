package trust

import (
	"math"
	"reflect"
	"testing"
)

// floatNear reports whether two accumulated float costs are equal within a small
// tolerance — additive float sums (0.10+0.20) are not bit-exact, so cost is
// asserted loosely while counts stay exact.
func floatNear(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// TestRecordClasslessIsGlobalByteIdentical is the core default-off guarantee:
// recording outcomes WITHOUT a Class must leave the global per-backend scoreboard
// (Snapshot.Backends, Rank, Order) byte-identical to a ledger that never knew
// about the class dimension. The empty class "" is the cell view of that exact
// scoreboard, so its rollups equal the global ones.
func TestRecordClasslessIsGlobalByteIdentical(t *testing.T) {
	outcomes := []Outcome{
		{Backend: "native", Passed: true},
		{Backend: "native", Passed: true},
		{Backend: "native", Passed: false},
		{Backend: "codex", Passed: true},
		{Backend: "claude", Passed: false},
		{Backend: "claude", Passed: true},
	}
	l := New()
	for _, o := range outcomes {
		l.Record(o)
	}
	snap := l.Snapshot()

	// The global backend scoreboard is exactly what it was before the class cell
	// existed: native 2/3, codex 1/1, claude 1/2.
	want := map[string]Stat{
		"native": {Backend: "native", Races: 3, Wins: 2, PassRate: 2.0 / 3.0},
		"codex":  {Backend: "codex", Races: 1, Wins: 1, PassRate: 1.0},
		"claude": {Backend: "claude", Races: 2, Wins: 1, PassRate: 0.5},
	}
	if len(snap.Backends) != len(want) {
		t.Fatalf("snapshot has %d backends, want %d", len(snap.Backends), len(want))
	}
	for _, got := range snap.Backends {
		if got != want[got.Backend] {
			t.Errorf("stat for %q = %+v, want %+v", got.Backend, got, want[got.Backend])
		}
	}

	// The "" class cell view mirrors the global rollup exactly (same races/wins/
	// rate), proving the empty class reproduces today's per-backend ledger.
	classless := l.ClassStandings("")
	gotCell := map[string]ClassStat{}
	for _, c := range classless {
		if c.Class != "" {
			t.Errorf("ClassStandings(\"\") returned a non-empty class: %+v", c)
		}
		gotCell[c.Backend] = c
	}
	for be, ws := range want {
		c := gotCell[be]
		if c.Races != ws.Races || c.Wins != ws.Wins || c.PassRate != ws.PassRate {
			t.Errorf("\"\" cell for %q = %+v, want races=%d wins=%d rate=%v",
				be, c, ws.Races, ws.Wins, ws.PassRate)
		}
		if c.TotalCost != 0 {
			t.Errorf("\"\" cell for %q has cost %v, want 0 (no cost recorded)", be, c.TotalCost)
		}
	}

	// And a class that was never recorded has no standing at all.
	if got := l.ClassStandings("refactor"); got != nil {
		t.Errorf("ClassStandings of an unseen class = %v, want nil", got)
	}
}

// TestRecordPerClassRoundTrip: outcomes recorded under distinct classes fold into
// distinct cells, accumulate cost additively, and DO NOT cross-contaminate. The
// global scoreboard still sums across classes.
func TestRecordPerClassRoundTrip(t *testing.T) {
	l := New()
	// native is great at refactors, mediocre at bugfixes.
	l.Record(Outcome{Backend: "native", Class: "refactor", Passed: true, Cost: 0.10})
	l.Record(Outcome{Backend: "native", Class: "refactor", Passed: true, Cost: 0.20})
	l.Record(Outcome{Backend: "native", Class: "bugfix", Passed: false, Cost: 0.50})
	// codex only ran a bugfix.
	l.Record(Outcome{Backend: "codex", Class: "bugfix", Passed: true, Cost: 0.05})

	// refactor cell: only native, 2/2, cost 0.30 (float sum, so compare loosely).
	ref := l.ClassStandings("refactor")
	if len(ref) != 1 {
		t.Fatalf("refactor standings = %+v, want 1 cell", ref)
	}
	if c := ref[0]; c.Class != "refactor" || c.Backend != "native" || c.Races != 2 || c.Wins != 2 || c.PassRate != 1.0 || !floatNear(c.TotalCost, 0.30) {
		t.Errorf("refactor cell = %+v, want native 2/2 cost≈0.30", c)
	}

	// bugfix cell: native 0/1 cost 0.50, codex 1/1 cost 0.05; codex ranks first
	// (smoothed score), native second.
	bug := l.ClassStandings("bugfix")
	if len(bug) != 2 {
		t.Fatalf("bugfix standings = %+v, want 2 cells", bug)
	}
	if bug[0].Backend != "codex" || bug[1].Backend != "native" {
		t.Errorf("bugfix order = %q,%q, want codex,native (smoothed score)", bug[0].Backend, bug[1].Backend)
	}
	gotBug := map[string]ClassStat{bug[0].Backend: bug[0], bug[1].Backend: bug[1]}
	if c := gotBug["native"]; c.Races != 1 || c.Wins != 0 || !floatNear(c.TotalCost, 0.50) {
		t.Errorf("bugfix native cell = %+v, want races=1 wins=0 cost≈0.50", c)
	}
	if c := gotBug["codex"]; c.Races != 1 || c.Wins != 1 || !floatNear(c.TotalCost, 0.05) {
		t.Errorf("bugfix codex cell = %+v, want races=1 wins=1 cost≈0.05", c)
	}

	// Global scoreboard sums across classes: native ran 3 (refactor 2 + bugfix 1),
	// won 2; codex ran 1, won 1.
	got := map[string]Stat{}
	for _, s := range l.Snapshot().Backends {
		got[s.Backend] = s
	}
	if n := got["native"]; n.Races != 3 || n.Wins != 2 {
		t.Errorf("global native = %+v, want races=3 wins=2", n)
	}
	if c := got["codex"]; c.Races != 1 || c.Wins != 1 {
		t.Errorf("global codex = %+v, want races=1 wins=1", c)
	}
}

// TestRecordClassFoldIsOrderIndependent: the per-class fold is commutative, so a
// Snapshot's Classes slice is identical regardless of insertion order.
func TestRecordClassFoldIsOrderIndependent(t *testing.T) {
	outcomes := []Outcome{
		{Backend: "native", Class: "refactor", Passed: true, Cost: 0.1},
		{Backend: "codex", Class: "refactor", Passed: false, Cost: 0.2},
		{Backend: "native", Class: "bugfix", Passed: true, Cost: 0.3},
		{Backend: "codex", Class: "bugfix", Passed: true, Cost: 0.4},
	}
	l1 := New()
	for _, o := range outcomes {
		l1.Record(o)
	}
	l2 := New()
	for i := len(outcomes) - 1; i >= 0; i-- {
		l2.Record(outcomes[i])
	}
	if !reflect.DeepEqual(l1.Snapshot().Classes, l2.Snapshot().Classes) {
		t.Errorf("class fold order-dependent:\n%+v\n%+v", l1.Snapshot().Classes, l2.Snapshot().Classes)
	}
}

// TestSnapshotClassesSorted: Snapshot.Classes is grouped by class name, then
// best-first by smoothed score within a class, ties broken by backend name.
func TestSnapshotClassesSorted(t *testing.T) {
	l := New()
	// Two classes, "refactor" before "test" by name. Within each, ensure the
	// stronger backend sorts first.
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "strong", Class: "test", Passed: true})
	}
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "weak", Class: "test", Passed: i < 2})
	}
	l.Record(Outcome{Backend: "beta", Class: "refactor", Passed: true})
	l.Record(Outcome{Backend: "beta", Class: "refactor", Passed: false})
	l.Record(Outcome{Backend: "alpha", Class: "refactor", Passed: true})
	l.Record(Outcome{Backend: "alpha", Class: "refactor", Passed: false})

	classes := l.Snapshot().Classes
	var order [][2]string
	for _, c := range classes {
		order = append(order, [2]string{c.Class, c.Backend})
	}
	want := [][2]string{
		{"refactor", "alpha"}, // tie on score ⇒ name tiebreak
		{"refactor", "beta"},
		{"test", "strong"}, // 10/10 beats 2/10
		{"test", "weak"},
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("Classes order = %v, want %v", order, want)
	}
}
