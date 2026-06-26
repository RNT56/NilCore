package experience_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/experience"
)

// writeLog builds a real, hash-chained event log with the given race_outcome
// rows so the replay runs against a valid chain (the chain check is the
// authority on validity — we never hand-roll the hashes).
func writeLog(t *testing.T, rows []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	lg, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, d := range rows {
		backend, _ := d["backend"].(string)
		lg.Append(eventlog.Event{Kind: "race_outcome", Backend: backend, Detail: d})
	}
	// a non-race event must be ignored by the fold.
	lg.Append(eventlog.Event{Kind: "model_call", Detail: map[string]any{"note": "ignored"}})
	if err := lg.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return path
}

func TestOverLogFoldsVerifierVerdicts(t *testing.T) {
	path := writeLog(t, []map[string]any{
		{"backend": "native", "passed": true, "cost": 0.10, "latency_ns": 1000.0},
		{"backend": "native", "passed": true, "cost": 0.30, "latency_ns": 3000.0},
		{"backend": "native", "passed": false, "cost": 0.20, "latency_ns": 2000.0},
		{"backend": "codex", "passed": false},
	})
	ctx := context.Background()
	x, err := experience.OverLog(path)
	if err != nil {
		t.Fatalf("OverLog: %v", err)
	}

	// Outcomes: 4 contests, 2 passes; median cost over {0.10,0.20,0.30} = 0.20;
	// median latency over {1000,2000,3000} = 2000.
	agg, _ := x.Outcomes(ctx, "go-refactor")
	if agg.Class != "go-refactor" {
		t.Errorf("Class = %q, want echoed taskClass", agg.Class)
	}
	if agg.Races != 4 || agg.Passes != 2 {
		t.Errorf("races/passes = %d/%d, want 4/2", agg.Races, agg.Passes)
	}
	if agg.MedianCostUSD != 0.20 {
		t.Errorf("median cost = %v, want 0.20", agg.MedianCostUSD)
	}
	if agg.MedianLatency != 2000 {
		t.Errorf("median latency = %v, want 2000", agg.MedianLatency)
	}
	if agg.LastSeen.IsZero() {
		t.Errorf("LastSeen should be set from the event times")
	}

	// BackendStanding: native has 3 races / 2 wins; codex 1 race / 0 wins.
	stands, _ := x.BackendStanding(ctx, "go-refactor")
	got := map[string][2]int{}
	for _, s := range stands {
		got[s.Backend] = [2]int{s.Races, s.Wins}
	}
	if got["native"] != [2]int{3, 2} {
		t.Errorf("native standing = %v, want races=3 wins=2", got["native"])
	}
	if got["codex"] != [2]int{1, 0} {
		t.Errorf("codex standing = %v, want races=1 wins=0", got["codex"])
	}

	ok, _ := x.ChainVerified(ctx)
	if !ok {
		t.Errorf("ChainVerified = false over a valid chain")
	}
	// log-only reader has no memory backend.
	if recs, _ := x.Lessons(ctx, "global", "", "", 0); len(recs) != 0 {
		t.Errorf("Lessons over a log-only reader = %d, want 0", len(recs))
	}
}

func TestOverLogSelfReportNeverPasses(t *testing.T) {
	// an event with no "passed" verdict must fold as a non-pass (I2: absent
	// evidence never counts as a win).
	path := writeLog(t, []map[string]any{
		{"backend": "native", "self_claimed": true}, // no "passed" key
	})
	x, err := experience.OverLog(path)
	if err != nil {
		t.Fatalf("OverLog: %v", err)
	}
	agg, _ := x.Outcomes(context.Background(), "")
	if agg.Races != 1 || agg.Passes != 0 {
		t.Errorf("races/passes = %d/%d, want 1/0 (self-claim is not a pass)", agg.Races, agg.Passes)
	}
}

func TestOverLogMissingLogIsCleanEmpty(t *testing.T) {
	x, err := experience.OverLog(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing log should be a clean empty reader, got err %v", err)
	}
	ctx := context.Background()
	if stands, _ := x.BackendStanding(ctx, ""); len(stands) != 0 {
		t.Errorf("missing-log standings = %d, want 0", len(stands))
	}
	if agg, _ := x.Outcomes(ctx, ""); agg.Races != 0 {
		t.Errorf("missing-log races = %d, want 0", agg.Races)
	}
	if ok, _ := x.ChainVerified(ctx); !ok {
		t.Errorf("missing (empty) log should read as chain-verified (vacuously)")
	}
}

func TestOverLogFailsClosedOnBrokenChain(t *testing.T) {
	path := writeLog(t, []map[string]any{
		{"backend": "native", "passed": true},
	})
	// Tamper: append a forged line whose hash does not link the chain.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for tamper: %v", err)
	}
	if _, err := f.WriteString(`{"seq":99,"kind":"race_outcome","backend":"native","detail":{"passed":true},"prev":"deadbeef","hash":"forged"}` + "\n"); err != nil {
		t.Fatalf("tamper write: %v", err)
	}
	f.Close()

	x, err := experience.OverLog(path)
	if err == nil {
		t.Fatalf("OverLog over a broken chain must error (fail-closed), got reader %+v", x)
	}
	if x != nil {
		t.Errorf("broken-chain OverLog must return a nil reader, got %+v", x)
	}
}
