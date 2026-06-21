package trust

import (
	"reflect"
	"testing"

	"nilcore/eval"
)

// TestRecordSnapshotDeterminism folds the same outcomes in different insertion
// orders and asserts the Snapshot is identical: the ledger is order-independent
// (a fold) and the snapshot is deterministically sorted best-first.
func TestRecordSnapshotDeterminism(t *testing.T) {
	outcomes := []Outcome{
		{Backend: "native", Passed: true},
		{Backend: "native", Passed: true},
		{Backend: "native", Passed: false},
		{Backend: "codex", Passed: true},
		{Backend: "claude", Passed: false},
		{Backend: "claude", Passed: true},
	}

	l1 := New()
	for _, o := range outcomes {
		l1.Record(o)
	}
	// Record the same set in reverse — the fold must be commutative.
	l2 := New()
	for i := len(outcomes) - 1; i >= 0; i-- {
		l2.Record(outcomes[i])
	}

	s1, s2 := l1.Snapshot(), l2.Snapshot()
	if !reflect.DeepEqual(s1, s2) {
		t.Fatalf("snapshot order-dependent:\n%+v\n%+v", s1, s2)
	}

	// native: 2/3, codex: 1/1, claude: 1/2. Check the raw stats are right.
	want := map[string]Stat{
		"native": {Backend: "native", Races: 3, Wins: 2, PassRate: 2.0 / 3.0},
		"codex":  {Backend: "codex", Races: 1, Wins: 1, PassRate: 1.0},
		"claude": {Backend: "claude", Races: 2, Wins: 1, PassRate: 0.5},
	}
	for _, got := range s1.Backends {
		w, ok := want[got.Backend]
		if !ok {
			t.Errorf("unexpected backend %q in snapshot", got.Backend)
			continue
		}
		if got != w {
			t.Errorf("stat for %q = %+v, want %+v", got.Backend, got, w)
		}
	}
	if len(s1.Backends) != len(want) {
		t.Errorf("snapshot has %d backends, want %d", len(s1.Backends), len(want))
	}
}

// TestRecordIgnoresEmptyBackend: a race_outcome with no attributable backend
// carries no signal and must not create a phantom row.
func TestRecordIgnoresEmptyBackend(t *testing.T) {
	l := New()
	l.Record(Outcome{Backend: "", Passed: true})
	if got := len(l.Snapshot().Backends); got != 0 {
		t.Errorf("empty-backend outcome created %d rows, want 0", got)
	}
}

// TestRankSmoothing is the core trust property: a 1-of-1 backend must NOT outrank
// a 90-of-100 one. Raw pass rate would tie them at 1.0 vs 0.9 (the lucky one
// wins); the smoothed score must invert that.
func TestRankSmoothing(t *testing.T) {
	l := New()
	// "lucky": a single win — raw rate 100%.
	l.Record(Outcome{Backend: "lucky", Passed: true})
	// "proven": 90 of 100 — raw rate 90%.
	for i := 0; i < 100; i++ {
		l.Record(Outcome{Backend: "proven", Passed: i < 90})
	}

	rank := l.Rank()
	if len(rank) != 2 {
		t.Fatalf("Rank = %v, want 2 entries", rank)
	}
	if rank[0] != "proven" {
		t.Errorf("Rank[0] = %q, want \"proven\" (smoothing must beat a 1-of-1 lucky sample)", rank[0])
	}

	// Be explicit about the scores so the property is documented, not incidental.
	if got := score(Stat{Backend: "lucky", Races: 1, Wins: 1}); got >= score(Stat{Backend: "proven", Races: 100, Wins: 90}) {
		t.Errorf("smoothed score(1/1)=%v should be < score(90/100)=%v",
			got, score(Stat{Races: 100, Wins: 90}))
	}
}

// TestRankZeroRacesIsPrior: a backend recorded with no wins and no... well, a
// fresh ledger has no backends at all. But a backend folded with a single loss
// should sort below the 0.5 prior, and the empty ledger ranks to nil.
func TestRankEmptyLedger(t *testing.T) {
	if got := New().Rank(); got != nil {
		t.Errorf("Rank of empty ledger = %v, want nil", got)
	}
}

// TestRankTieBreakByName: equal smoothed scores break deterministically by name.
func TestRankTieBreakByName(t *testing.T) {
	l := New()
	// Both 1-of-2 ⇒ identical smoothed score; "alpha" must precede "beta".
	for _, n := range []string{"beta", "alpha"} {
		l.Record(Outcome{Backend: n, Passed: true})
		l.Record(Outcome{Backend: n, Passed: false})
	}
	rank := l.Rank()
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(rank, want) {
		t.Errorf("Rank = %v, want %v (ties break by name)", rank, want)
	}
}

// TestOrder: known backends sort best-first; unknown ones sort last in input
// order; the input slice is not mutated.
func TestOrder(t *testing.T) {
	l := New()
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "strong", Passed: true}) // 10/10
	}
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "weak", Passed: i < 2}) // 2/10
	}

	input := []string{"weak", "unknownB", "strong", "unknownA"}
	got := l.Order(input)
	want := []string{"strong", "weak", "unknownB", "unknownA"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Order = %v, want %v", got, want)
	}
	// Input must be untouched.
	if input[0] != "weak" || input[2] != "strong" {
		t.Errorf("Order mutated its input: %v", input)
	}
}

// TestFoldEvalReport folds a report and asserts the per-config rollup; re-folding
// the same config name overwrites (snapshots, not increments).
func TestFoldEvalReport(t *testing.T) {
	l := New()
	r := eval.Report{
		Config:    "native+opus",
		PassRate:  0.8,
		TotalCost: 1.25,
		Results:   make([]eval.Result, 5),
	}
	l.FoldEvalReport(r)

	cfgs := l.Snapshot().Configs
	if len(cfgs) != 1 {
		t.Fatalf("Configs = %+v, want 1", cfgs)
	}
	want := ConfigStat{Config: "native+opus", PassRate: 0.8, TotalCost: 1.25, Cases: 5}
	if cfgs[0] != want {
		t.Errorf("ConfigStat = %+v, want %+v", cfgs[0], want)
	}

	// Re-fold the SAME config with new numbers — latest measurement wins.
	l.FoldEvalReport(eval.Report{Config: "native+opus", PassRate: 0.9, TotalCost: 2.0, Results: make([]eval.Result, 10)})
	cfgs = l.Snapshot().Configs
	if len(cfgs) != 1 {
		t.Fatalf("re-fold should overwrite, got %d configs", len(cfgs))
	}
	if cfgs[0].PassRate != 0.9 || cfgs[0].Cases != 10 {
		t.Errorf("re-fold did not overwrite: %+v", cfgs[0])
	}
}

// TestFoldEvalReportIgnoresEmptyConfig: a report with no config name has nothing
// to attribute and is dropped.
func TestFoldEvalReportIgnoresEmptyConfig(t *testing.T) {
	l := New()
	l.FoldEvalReport(eval.Report{Config: "", PassRate: 1.0})
	if got := len(l.Snapshot().Configs); got != 0 {
		t.Errorf("empty-config report folded %d configs, want 0", got)
	}
}

// TestConfigsSortedByName: snapshot configs are deterministically name-sorted.
func TestConfigsSortedByName(t *testing.T) {
	l := New()
	for _, c := range []string{"zeta", "alpha", "mid"} {
		l.FoldEvalReport(eval.Report{Config: c, Results: make([]eval.Result, 1)})
	}
	cfgs := l.Snapshot().Configs
	got := []string{cfgs[0].Config, cfgs[1].Config, cfgs[2].Config}
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config order = %v, want %v", got, want)
	}
}
