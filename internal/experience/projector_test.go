package experience_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/experience"
	"nilcore/internal/store"
	"nilcore/internal/trust"
)

func standingMap(ss []trust.Stat) map[string][2]int {
	m := map[string][2]int{}
	for _, s := range ss {
		m[s.Backend] = [2]int{s.Races, s.Wins}
	}
	return m
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProjectorRebuildAndOverStoreParity(t *testing.T) {
	ctx := context.Background()
	path := writeLog(t, []map[string]any{
		{"backend": "native", "passed": true, "cost": 0.10, "latency_ns": 1000.0},
		{"backend": "native", "passed": true, "cost": 0.30, "latency_ns": 3000.0},
		{"backend": "native", "passed": false},
		{"backend": "codex", "passed": false},
	})
	s := openStore(t)
	if err := experience.NewProjector(s).Rebuild(ctx, path); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rd := experience.OverStore(s, nil)

	got, _ := rd.BackendStanding(ctx, "")
	gm := standingMap(got)
	if gm["native"] != [2]int{3, 2} || gm["codex"] != [2]int{1, 0} {
		t.Fatalf("store standings = %v, want native 3/2, codex 1/0", gm)
	}

	// Parity: the store-backed reader and the log-only reader agree.
	logRd, _ := experience.OverLog(path)
	logSt, _ := logRd.BackendStanding(ctx, "")
	if lm := standingMap(logSt); lm["native"] != gm["native"] || lm["codex"] != gm["codex"] {
		t.Errorf("OverStore and OverLog disagree: store=%v log=%v", gm, lm)
	}

	if ok, _ := rd.ChainVerified(ctx); !ok {
		t.Errorf("ChainVerified should be true over a valid chain")
	}

	// Idempotent: a second Rebuild over the same (append-only) log is unchanged.
	if err := experience.NewProjector(s).Rebuild(ctx, path); err != nil {
		t.Fatalf("rebuild again: %v", err)
	}
	again, _ := rd.BackendStanding(ctx, "")
	if standingMap(again)["native"] != [2]int{3, 2} {
		t.Errorf("rebuild not idempotent: %v", standingMap(again))
	}
}

func TestProjectorFoldIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	p := experience.NewProjector(s)
	ev := eventlog.Event{Seq: 1, Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": true}}
	if err := p.Fold(ctx, ev); err != nil {
		t.Fatalf("fold: %v", err)
	}
	// Folding the SAME event (seq 1) again is a no-op via the watermark.
	if err := p.Fold(ctx, ev); err != nil {
		t.Fatalf("fold repeat: %v", err)
	}
	st, _ := experience.OverStore(s, nil).BackendStanding(ctx, "")
	if standingMap(st)["native"] != [2]int{1, 1} {
		t.Errorf("double-fold not idempotent: %v", standingMap(st))
	}
}

func TestProjectorFailsClosedOnBrokenChain(t *testing.T) {
	ctx := context.Background()
	path := writeLog(t, []map[string]any{{"backend": "native", "passed": true}})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for tamper: %v", err)
	}
	if _, err := f.WriteString(`{"seq":99,"kind":"race_outcome","backend":"native","detail":{"passed":true},"prev":"x","hash":"forged"}` + "\n"); err != nil {
		t.Fatalf("tamper write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s := openStore(t)
	if err := experience.NewProjector(s).Rebuild(ctx, path); err == nil {
		t.Fatalf("rebuild over a broken chain must error (fail-closed)")
	}
	// The watermark records the broken chain so a reader fails closed too.
	if ok, _ := experience.OverStore(s, nil).ChainVerified(ctx); ok {
		t.Errorf("ChainVerified must be false after a broken-chain rebuild")
	}
}
