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

// TestActivationViaOnAppendHook exercises the EXP-T03 activation path end-to-end:
// a live eventlog wired with OnAppend(proj.Fold) (exactly as cmd's wireExperience
// does) keeps the store projection warm as events land — no manual Rebuild needed.
func TestActivationViaOnAppendHook(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "live.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	s := openStore(t)
	log.UseStore(s)
	proj := experience.NewProjector(s)
	log.OnAppend(func(e eventlog.Event) { _ = proj.Fold(ctx, e) })

	// A non-race_outcome event must NOT change any standing (I2: only verifier verdicts
	// fold) — and it takes seq 0 so the first race_outcome lands above the watermark.
	log.Append(eventlog.Event{Kind: "task_start", Backend: "native"})
	log.Append(eventlog.Event{Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": true}})
	log.Append(eventlog.Event{Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": false}})
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// OverStore reflects the folded outcomes WITHOUT a manual Rebuild — the hook kept
	// the projection warm as the events landed.
	st, _ := experience.OverStore(s, nil).BackendStanding(ctx, "")
	if standingMap(st)["native"] != [2]int{2, 1} {
		t.Fatalf("live projection = %v, want native 2 races / 1 win", standingMap(st))
	}
}

// TestProjectorConfigStanding asserts the projector folds selfeval_report events
// into exp_config_standing so `experience -warm` surfaces per-config standings
// (both via Rebuild and the incremental Fold hook).
func TestProjectorConfigStanding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.jsonl")
	lg, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	lg.Append(eventlog.Event{Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": true}})
	lg.Append(eventlog.Event{Kind: "selfeval_report", Detail: map[string]any{"config": "opus-native", "pass_rate": 0.8, "cases": 5.0}})
	// A later report for the SAME config overwrites (snapshots, latest wins).
	lg.Append(eventlog.Event{Kind: "selfeval_report", Detail: map[string]any{"config": "opus-native", "pass_rate": 0.9, "cases": 10.0}})
	lg.Append(eventlog.Event{Kind: "selfeval_report", Detail: map[string]any{"config": "sonnet-codex", "pass_rate": 0.5, "cases": 4.0}})
	// A config-less report attributes to nothing (skipped).
	lg.Append(eventlog.Event{Kind: "selfeval_report", Detail: map[string]any{"pass_rate": 1.0, "cases": 1.0}})
	if err := lg.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	s := openStore(t)
	if err := experience.NewProjector(s).Rebuild(ctx, path); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	configs, err := experience.OverStore(s, nil).ConfigStanding(ctx)
	if err != nil {
		t.Fatalf("config standing: %v", err)
	}
	cm := map[string]trust.ConfigStat{}
	for _, c := range configs {
		cm[c.Config] = c
	}
	if len(configs) != 2 {
		t.Fatalf("config standings = %d, want 2 (config-less skipped), got %v", len(configs), cm)
	}
	if got := cm["opus-native"]; got.PassRate != 0.9 || got.Cases != 10 {
		t.Errorf("opus-native = %+v, want pass_rate 0.9 cases 10 (latest wins)", got)
	}
	if got := cm["sonnet-codex"]; got.PassRate != 0.5 || got.Cases != 4 {
		t.Errorf("sonnet-codex = %+v, want pass_rate 0.5 cases 4", got)
	}
}

// TestProjectorClassStandings asserts the projector keys backend standings by the
// race's real class so `experience -warm -class code` returns rows (the warm path
// agrees with the log-replay path).
func TestProjectorClassStandings(t *testing.T) {
	ctx := context.Background()
	path := writeLog(t, []map[string]any{
		{"backend": "native", "passed": true, "class": "code"},
		{"backend": "native", "passed": false, "class": "code"},
		{"backend": "codex", "passed": true, "class": "docs"},
	})
	s := openStore(t)
	if err := experience.NewProjector(s).Rebuild(ctx, path); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rd := experience.OverStore(s, nil)

	// Global ("") holds every race.
	gl, _ := rd.BackendStanding(ctx, "")
	if standingMap(gl)["native"] != [2]int{2, 1} || standingMap(gl)["codex"] != [2]int{1, 1} {
		t.Fatalf("global = %v, want native 2/1 codex 1/1", standingMap(gl))
	}
	// class=code returns only the native/code races — the finding's failing case.
	code, _ := rd.BackendStanding(ctx, "code")
	if len(code) != 1 || standingMap(code)["native"] != [2]int{2, 1} {
		t.Fatalf("class=code = %v, want only native 2/1", standingMap(code))
	}
	if agg, _ := rd.Outcomes(ctx, "code"); agg.Races != 2 || agg.Passes != 1 {
		t.Errorf("class=code outcomes = %d/%d, want 2/1", agg.Races, agg.Passes)
	}

	// Parity with the log-replay path on the same class filter.
	logRd, _ := experience.OverLog(path)
	logCode, _ := logRd.BackendStanding(ctx, "code")
	if standingMap(logCode)["native"] != standingMap(code)["native"] {
		t.Errorf("warm and log disagree on class=code: warm=%v log=%v", standingMap(code), standingMap(logCode))
	}
}

// TestProjectorFoldClassIncremental exercises the incremental Fold hook keying by
// class (the live-activation path), not just Rebuild.
func TestProjectorFoldClassIncremental(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	p := experience.NewProjector(s)
	evs := []eventlog.Event{
		{Seq: 0, Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": true, "class": "code"}},
		{Seq: 1, Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": false, "class": "code"}},
		{Seq: 2, Kind: "selfeval_report", Detail: map[string]any{"config": "c1", "pass_rate": 0.7, "cases": 3.0}},
	}
	for _, ev := range evs {
		if err := p.Fold(ctx, ev); err != nil {
			t.Fatalf("fold %d: %v", ev.Seq, err)
		}
	}
	rd := experience.OverStore(s, nil)
	if code, _ := rd.BackendStanding(ctx, "code"); standingMap(code)["native"] != [2]int{2, 1} {
		t.Errorf("incremental class=code = %v, want native 2/1", standingMap(code))
	}
	if gl, _ := rd.BackendStanding(ctx, ""); standingMap(gl)["native"] != [2]int{2, 1} {
		t.Errorf("incremental global = %v, want native 2/1", standingMap(gl))
	}
	configs, _ := rd.ConfigStanding(ctx)
	if len(configs) != 1 || configs[0].Config != "c1" || configs[0].Cases != 3 {
		t.Errorf("incremental configs = %v, want c1 cases 3", configs)
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

// TestProjectorFoldSeqZero guards the watermark edge surfaced by live activation: a
// race_outcome that is the literal first log event (seq 0) must fold on a fresh
// projection, not be dropped by a spurious 0 <= 0 watermark comparison.
func TestProjectorFoldSeqZero(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	p := experience.NewProjector(s)
	ev := eventlog.Event{Seq: 0, Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": true}}
	if err := p.Fold(ctx, ev); err != nil {
		t.Fatalf("fold seq 0: %v", err)
	}
	st, _ := experience.OverStore(s, nil).BackendStanding(ctx, "")
	if standingMap(st)["native"] != [2]int{1, 1} {
		t.Fatalf("seq-0 race_outcome must fold on a fresh projection, got %v", standingMap(st))
	}
	// Folding the same seq-0 event again is a no-op (a meta row now exists ⇒ 0 <= 0).
	if err := p.Fold(ctx, ev); err != nil {
		t.Fatalf("re-fold seq 0: %v", err)
	}
	st2, _ := experience.OverStore(s, nil).BackendStanding(ctx, "")
	if standingMap(st2)["native"] != [2]int{1, 1} {
		t.Fatalf("seq-0 re-fold must be idempotent, got %v", standingMap(st2))
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
