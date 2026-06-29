package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/eval"
	"nilcore/internal/eventlog"
	"nilcore/internal/trust"
)

// seedTrustLog writes a hash-chained event log with verifier-judged race_outcome
// events: backend "alpha" wins both its races, "beta" loses both. trust.Replay
// folds these into the per-backend scoreboard, and the chain verifies (written
// through the real eventlog writer). Returns the log path.
func seedTrustLog(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Task: "t-1", Backend: "alpha", Kind: "race_outcome", Detail: map[string]any{"passed": true}})
	log.Append(eventlog.Event{Task: "t-1", Backend: "beta", Kind: "race_outcome", Detail: map[string]any{"passed": false}})
	log.Append(eventlog.Event{Task: "t-2", Backend: "alpha", Kind: "race_outcome", Detail: map[string]any{"passed": true}})
	log.Append(eventlog.Event{Task: "t-2", Backend: "beta", Kind: "race_outcome", Detail: map[string]any{"passed": false}})
	log.Close()
	return logPath
}

// breakTrustChain corrupts the hash chain by appending a forged line directly to
// the file (bypassing Append), so eventlog.Verify fails and trust.Replay must
// fail-closed (error, no ledger).
func breakTrustChain(t *testing.T, logPath string) {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":99,"kind":"race_outcome","backend":"alpha","detail":{"passed":true},"hash":"deadbeef"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTrustCleanLogTextScoreboard(t *testing.T) {
	logPath := seedTrustLog(t)
	out, err := runTrust(logPath, "text", "", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrust: %v", err)
	}
	// Both backends appear with their verifier-judged tallies. alpha (2/2) ranks
	// above beta (0/2); the header carries the I2 reminder.
	for _, want := range []string{"alpha", "beta", "Trust Ledger"} {
		if !strings.Contains(out, want) {
			t.Errorf("scoreboard missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "verifier still decides") {
		t.Errorf("scoreboard missing the I2 reminder:\n%s", out)
	}
	// alpha must be rendered before beta (best-first ordering).
	if strings.Index(out, "alpha") > strings.Index(out, "beta") {
		t.Errorf("alpha should rank above beta:\n%s", out)
	}
}

func TestTrustJSONSnapshot(t *testing.T) {
	logPath := seedTrustLog(t)
	out, err := runTrust(logPath, "json", "", plainStyle(t))
	if err != nil {
		t.Fatalf("runTrust json: %v", err)
	}
	var snap trust.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v\n%s", err, out)
	}
	if len(snap.Backends) != 2 {
		t.Fatalf("want 2 backends in snapshot, got %d: %+v", len(snap.Backends), snap.Backends)
	}
	// Best-first: alpha (2 wins / 2 races) leads beta (0 / 2).
	if snap.Backends[0].Backend != "alpha" {
		t.Errorf("want alpha first, got %q", snap.Backends[0].Backend)
	}
	if snap.Backends[0].Wins != 2 || snap.Backends[0].Races != 2 {
		t.Errorf("alpha stats wrong: %+v", snap.Backends[0])
	}
}

func TestTrustBrokenChainFailsClosed(t *testing.T) {
	logPath := seedTrustLog(t)
	breakTrustChain(t, logPath)
	// A tampered log yields NO trustworthy ranking: runTrust returns an error (the
	// caller, trustMain, then exits non-zero). It must NOT silently rank.
	if _, err := runTrust(logPath, "text", "", plainStyle(t)); err == nil {
		t.Fatal("runTrust over a broken chain must error (fail-closed), got nil")
	}
}

func TestTrustMissingLogIsEmptyLedger(t *testing.T) {
	// A missing log is a fresh install with no history — a clean empty ledger, not a
	// failure. The scoreboard renders the "defers to the default" line.
	out, err := runTrust(filepath.Join(t.TempDir(), "absent.jsonl"), "text", "", plainStyle(t))
	if err != nil {
		t.Fatalf("missing log should be a clean empty ledger, got: %v", err)
	}
	if !strings.Contains(out, "no earned outcomes") {
		t.Errorf("empty ledger should note no earned outcomes:\n%s", out)
	}
}

func TestTrustEvalFold(t *testing.T) {
	logPath := seedTrustLog(t)
	// Write an eval report and fold it; its config row must surface in the scoreboard.
	rep := eval.Report{
		Config:    "native:claude",
		PassRate:  0.8,
		TotalCost: 1.25,
		Results:   []eval.Result{{Case: "c1", Passed: true}, {Case: "c2", Passed: false}},
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	evalPath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(evalPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTrust(logPath, "text", evalPath, plainStyle(t))
	if err != nil {
		t.Fatalf("runTrust with eval: %v", err)
	}
	for _, want := range []string{"eval configs", "native:claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("scoreboard missing eval fold %q:\n%s", want, out)
		}
	}
}

func TestTrustEvalJSONFold(t *testing.T) {
	logPath := seedTrustLog(t)
	rep := eval.Report{Config: "cfg-x", PassRate: 0.5, TotalCost: 0.4, Results: []eval.Result{{Case: "c"}}}
	data, _ := json.Marshal(rep)
	evalPath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(evalPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTrust(logPath, "json", evalPath, plainStyle(t))
	if err != nil {
		t.Fatalf("runTrust json+eval: %v", err)
	}
	var snap trust.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(snap.Configs) != 1 || snap.Configs[0].Config != "cfg-x" {
		t.Errorf("eval config not folded into snapshot: %+v", snap.Configs)
	}
}

func TestTrustBadEvalReportErrors(t *testing.T) {
	logPath := seedTrustLog(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTrust(logPath, "text", bad, plainStyle(t)); err == nil {
		t.Fatal("a malformed eval report must be a hard error, not a silent skip")
	}
}

func TestTrustUnknownFormatErrors(t *testing.T) {
	if _, err := runTrust(seedTrustLog(t), "yaml", "", plainStyle(t)); err == nil {
		t.Fatal("unknown -format must error")
	}
}
