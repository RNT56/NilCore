package experience_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// TestProjectorFoldResumesAfterRotation guards the rotation-awareness of the live Fold
// path. serve caps the append-only log at 64 MiB by moving it aside and starting a
// FRESH genesis chain at seq 0 (maint.RotateLog), whose low seqs land far below the
// high-water mark the rotated-away chain left in exp_meta. Fold must recognise that
// backward seq jump as a NEW CHAIN and re-derive from it — folding the new events (not
// silently dropping them as "already folded" until the new chain climbs past the old
// watermark ~64 MiB later) AND clearing the rotated-away generation's stale standings,
// so the warm projection equals a fresh fold of the current log (I5).
func TestProjectorFoldResumesAfterRotation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	p := experience.NewProjector(s)

	// Generation 0: a race lands with a HIGH seq (the log had climbed well past genesis
	// before rotation), advancing the watermark to 40.
	if err := p.Fold(ctx, eventlog.Event{Seq: 40, Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": true}}); err != nil {
		t.Fatalf("fold gen0: %v", err)
	}
	if st, _ := experience.OverStore(s, nil).BackendStanding(ctx, ""); standingMap(st)["native"] != [2]int{1, 1} {
		t.Fatalf("gen0 native = %v, want 1/1", standingMap(st))
	}

	// Generation 1 (post-rotation): the fresh chain restarts at seq 0 with a DIFFERENT
	// backend. seq 0 is below the watermark (40); the OLD code dropped it as already-
	// folded (0 <= 40). It must now fold, and the rotated-away native row must be cleared.
	if err := p.Fold(ctx, eventlog.Event{Seq: 0, Kind: "race_outcome", Backend: "codex", Detail: map[string]any{"passed": true}}); err != nil {
		t.Fatalf("fold gen1 genesis: %v", err)
	}
	gm := standingMap(mustStanding(ctx, t, s))
	if _, stale := gm["native"]; stale {
		t.Errorf("rotated-away native standing must be cleared, got %v", gm)
	}
	if gm["codex"] != [2]int{1, 1} {
		t.Errorf("post-rotation codex = %v, want 1/1 (fold resumed)", gm)
	}

	// Folding then continues normally on the new chain (seq 1 > the new watermark 0).
	if err := p.Fold(ctx, eventlog.Event{Seq: 1, Kind: "race_outcome", Backend: "codex", Detail: map[string]any{"passed": false}}); err != nil {
		t.Fatalf("fold gen1 seq1: %v", err)
	}
	if got := standingMap(mustStanding(ctx, t, s))["codex"]; got != [2]int{2, 1} {
		t.Errorf("post-rotation codex after 2 races = %v, want 2/1", got)
	}
}

// TestProjectorRebuildDropsStaleKeysAcrossGenerations guards the authoritative
// truncate-then-rebuild: after a rotation the live log is a fresh genesis chain, so a
// Rebuild over it must re-derive the WHOLE projection from THAT log — dropping
// (class, backend) keys earned only in a rotated-away generation. Otherwise the
// upsert-only rebuild would leave OverStore carrying keys OverLog (a fresh fold of the
// same log) does not, breaking their parity (I5).
func TestProjectorRebuildDropsStaleKeysAcrossGenerations(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	// Generation A: two backends earn standings.
	genA := writeLog(t, []map[string]any{
		{"backend": "native", "passed": true},
		{"backend": "codex", "passed": false},
	})
	if err := experience.NewProjector(s).Rebuild(ctx, genA); err != nil {
		t.Fatalf("rebuild genA: %v", err)
	}
	if st := mustStanding(ctx, t, s); len(st) != 2 {
		t.Fatalf("after genA, standings = %v, want 2 backends", standingMap(st))
	}

	// Generation B (post-rotation): a fresh genesis chain with a DIFFERENT backend only.
	// Rebuild over it must reflect ONLY genB — native and codex are gone.
	genB := writeLog(t, []map[string]any{
		{"backend": "claude-code", "passed": true},
	})
	if err := experience.NewProjector(s).Rebuild(ctx, genB); err != nil {
		t.Fatalf("rebuild genB: %v", err)
	}

	gm := standingMap(mustStanding(ctx, t, s))
	if _, stale := gm["native"]; stale {
		t.Errorf("native (rotated-away) must be dropped, got %v", gm)
	}
	if _, stale := gm["codex"]; stale {
		t.Errorf("codex (rotated-away) must be dropped, got %v", gm)
	}
	if gm["claude-code"] != [2]int{1, 1} {
		t.Errorf("claude-code = %v, want 1/1", gm)
	}

	// OverStore == OverLog over the CURRENT log — the I5 parity the fix restores.
	logRd, err := experience.OverLog(genB)
	if err != nil {
		t.Fatalf("OverLog genB: %v", err)
	}
	logSt, _ := logRd.BackendStanding(ctx, "")
	if lm := standingMap(logSt); !reflect.DeepEqual(lm, gm) {
		t.Errorf("OverStore != OverLog across generations: store=%v log=%v", gm, lm)
	}
}

// mustStanding reads the global ("") backend standings from the store-backed reader,
// failing the test on a query error.
func mustStanding(ctx context.Context, t *testing.T, s *store.Store) []trust.Stat {
	t.Helper()
	st, err := experience.OverStore(s, nil).BackendStanding(ctx, "")
	if err != nil {
		t.Fatalf("backend standing: %v", err)
	}
	return st
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
