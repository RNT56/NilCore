package selfeval_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nilcore/eval"
	"nilcore/internal/eventlog"
	"nilcore/internal/flywheel/selfeval"
	"nilcore/internal/trust"
)

// writeLog builds a real, hash-chained event log so the fold's chain check runs
// against a valid chain (eventlog.Verify is the authority — we never hand-roll the
// hashes). It returns the path to the JSONL log.
func writeLog(t *testing.T, kinds []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	lg, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, k := range kinds {
		lg.Append(eventlog.Event{Kind: k, Detail: map[string]any{"note": "seed"}})
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return path
}

// recordingSink captures appended events so a test can assert what the fold
// emitted without writing a file.
type recordingSink struct{ events []eventlog.Event }

func (s *recordingSink) Append(e eventlog.Event) { s.events = append(s.events, e) }

// verifierReport is a report the harness would produce: every Result.Passed is the
// objective verifier's verdict (eval.Run sets it from the Runner). config names the
// scored config; passes/total set the verifier-judged outcomes.
func verifierReport(config string, passes, total int) eval.Report {
	r := eval.Report{Config: config}
	for i := 0; i < total; i++ {
		r.Results = append(r.Results, eval.Result{
			Case:   "case",
			Config: config,
			Passed: i < passes, // first `passes` cases pass the verifier
		})
	}
	if total > 0 {
		r.PassRate = float64(passes) / float64(total)
	}
	return r
}

// TestFoldOverCleanChainFolds proves a verifier-judged report over a clean chain
// folds into the per-config trust scoreboard and emits one metadata-only event.
func TestFoldOverCleanChainFolds(t *testing.T) {
	path := writeLog(t, []string{"task_start", "verify"})
	led := trust.New()
	sink := &recordingSink{}

	rep := selfeval.NewVerifierJudged(verifierReport("native@gpt", 8, 10))
	res, err := selfeval.Fold(context.Background(), path, rep, led, sink)
	if err != nil {
		t.Fatalf("Fold over a clean chain must succeed, got %v", err)
	}
	if !res.ChainOK {
		t.Errorf("ChainOK = false over a valid chain")
	}
	if res.Config != "native@gpt" || res.Cases != 10 || res.Passes != 8 {
		t.Errorf("FoldResult = %+v, want config=native@gpt cases=10 passes=8", res)
	}
	if res.PassRate != 0.8 {
		t.Errorf("PassRate = %v, want 0.8", res.PassRate)
	}

	// The trust scoreboard now carries the config's verifier-judged standing.
	snap := led.Snapshot()
	var found bool
	for _, c := range snap.Configs {
		if c.Config == "native@gpt" {
			found = true
			if c.PassRate != 0.8 || c.Cases != 10 {
				t.Errorf("config standing = %+v, want passRate=0.8 cases=10", c)
			}
		}
	}
	if !found {
		t.Errorf("config standing was not folded into the ledger")
	}

	// Exactly one append-only selfeval_report event, metadata only.
	if len(sink.events) != 1 {
		t.Fatalf("emitted %d events, want exactly 1", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Kind != selfeval.EventKind {
		t.Errorf("event kind = %q, want %q", ev.Kind, selfeval.EventKind)
	}
	if ev.Detail["config"] != "native@gpt" || ev.Detail["passes"] != 8 || ev.Detail["cases"] != 10 {
		t.Errorf("event detail = %v, want structural config/passes/cases", ev.Detail)
	}
	if ev.Detail["pass_rate"] != 0.8 || ev.Detail["chain_ok"] != true {
		t.Errorf("event detail pass_rate/chain_ok = %v", ev.Detail)
	}
}

// TestFoldFailsClosedOnBrokenChain proves a tampered chain blocks the fold: the
// ledger is left untouched and no event is emitted (I5, fail-closed — a tampered
// log earns nothing).
func TestFoldFailsClosedOnBrokenChain(t *testing.T) {
	path := writeLog(t, []string{"task_start"})
	// Tamper: append a forged line whose hash does not link the chain.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for tamper: %v", err)
	}
	if _, err := f.WriteString(`{"seq":99,"kind":"verify","prev":"deadbeef","hash":"forged"}` + "\n"); err != nil {
		t.Fatalf("tamper write: %v", err)
	}
	f.Close()

	led := trust.New()
	sink := &recordingSink{}
	rep := selfeval.NewVerifierJudged(verifierReport("native@gpt", 10, 10))

	res, err := selfeval.Fold(context.Background(), path, rep, led, sink)
	if err == nil {
		t.Fatalf("Fold over a broken chain must fail-closed, got result %+v", res)
	}
	if !errors.Is(err, selfeval.ErrChainBroken) {
		t.Errorf("error = %v, want it to wrap ErrChainBroken", err)
	}
	// Nothing folded: the ledger has no config standing.
	if got := len(led.Snapshot().Configs); got != 0 {
		t.Errorf("ledger folded %d configs over a broken chain, want 0 (earns nothing)", got)
	}
	// Nothing emitted: a broken chain produces no audit record of a fold.
	if got := len(sink.events); got != 0 {
		t.Errorf("emitted %d events over a broken chain, want 0", got)
	}
}

// TestFoldRefusesSelfClaim proves the fold can only ever count VERIFIER verdicts.
// A report that is NOT verifier-judged (a self-generated report with no verifier
// boundary) is refused outright (I2 — the agent cannot inflate its own standing),
// and a verifier-judged report's pass count is exactly the verifier's, never a
// self-claim of more.
func TestFoldRefusesSelfClaim(t *testing.T) {
	path := writeLog(t, []string{"task_start"})
	led := trust.New()
	sink := &recordingSink{}

	// (a) An un-wrapped report (zero-value verifierJudged) is a self-claim: refused
	// before any state changes, even over a perfectly clean chain.
	var selfClaim selfeval.Report // zero value: NOT verifier-judged
	res, err := selfeval.Fold(context.Background(), path, selfClaim, led, sink)
	if err == nil {
		t.Fatalf("Fold of a non-verifier-judged report must be refused, got %+v", res)
	}
	if !errors.Is(err, selfeval.ErrNotVerifierJudged) {
		t.Errorf("error = %v, want ErrNotVerifierJudged", err)
	}
	if len(led.Snapshot().Configs) != 0 || len(sink.events) != 0 {
		t.Errorf("a refused self-claim must fold nothing and emit nothing")
	}

	// (b) Even a verifier-judged report only counts the verifier's passes. A report
	// where the verifier passed 3 of 10 folds exactly 3 — a self-claim of 10 has no
	// representation, because Passes() reads only eval.Result.Passed (the verifier
	// verdict). Confirm the folded pass count is the verifier's 3, not the total.
	rep := selfeval.NewVerifierJudged(verifierReport("native@gpt", 3, 10))
	res2, err := selfeval.Fold(context.Background(), path, rep, led, sink)
	if err != nil {
		t.Fatalf("verifier-judged fold over clean chain: %v", err)
	}
	if res2.Passes != 3 {
		t.Errorf("folded passes = %d, want the verifier's 3 (never a self-claimed inflation)", res2.Passes)
	}
	if res2.PassRate != 0.3 {
		t.Errorf("folded pass-rate = %v, want 0.3", res2.PassRate)
	}
}

// TestFoldNilSafe proves the default-off / nil-safe contract: a nil ledger folds
// nothing into trust but still verifies the chain and emits the honest record; a
// nil sink emits no event but still folds and verifies.
func TestFoldNilSafe(t *testing.T) {
	path := writeLog(t, []string{"task_start"})
	rep := selfeval.NewVerifierJudged(verifierReport("native@gpt", 5, 5))

	// nil ledger: no fold target, but the chain is verified and the event emitted.
	sink := &recordingSink{}
	if _, err := selfeval.Fold(context.Background(), path, rep, nil, sink); err != nil {
		t.Fatalf("Fold with nil ledger must succeed, got %v", err)
	}
	if len(sink.events) != 1 {
		t.Errorf("nil-ledger fold emitted %d events, want 1", len(sink.events))
	}

	// nil sink: no event emitted, but the chain is verified and the fold applied.
	led := trust.New()
	if _, err := selfeval.Fold(context.Background(), path, rep, led, nil); err != nil {
		t.Fatalf("Fold with nil sink must succeed, got %v", err)
	}
	if len(led.Snapshot().Configs) != 1 {
		t.Errorf("nil-sink fold folded %d configs, want 1", len(led.Snapshot().Configs))
	}
}

// TestFoldEmptyConfigFoldsNothingButVerifies proves a report with no Config name
// verifies the chain and emits the record but folds nothing into trust (the trust
// fold has nothing to attribute it to) — mirroring trust.Ledger.FoldEvalReport.
func TestFoldEmptyConfigFoldsNothingButVerifies(t *testing.T) {
	path := writeLog(t, []string{"task_start"})
	led := trust.New()
	sink := &recordingSink{}
	rep := selfeval.NewVerifierJudged(verifierReport("", 4, 5))

	res, err := selfeval.Fold(context.Background(), path, rep, led, sink)
	if err != nil {
		t.Fatalf("Fold with empty config must still succeed (chain verified): %v", err)
	}
	if !res.ChainOK {
		t.Errorf("ChainOK = false over a valid chain")
	}
	if len(led.Snapshot().Configs) != 0 {
		t.Errorf("empty-config report folded %d configs, want 0", len(led.Snapshot().Configs))
	}
	if len(sink.events) != 1 {
		t.Errorf("empty-config fold emitted %d events, want 1 (audit stays honest)", len(sink.events))
	}
}

// TestFoldMissingLogFailsClosed proves an empty/missing log path is refused: there
// is no chain to verify, so folding would defeat the I5 guard. (eventlog.Verify on
// a nonexistent path errors; an empty path is rejected up front.)
func TestFoldMissingLogFailsClosed(t *testing.T) {
	led := trust.New()
	rep := selfeval.NewVerifierJudged(verifierReport("native@gpt", 1, 1))

	// Empty path: rejected before any state change.
	if _, err := selfeval.Fold(context.Background(), "", rep, led, nil); err == nil {
		t.Errorf("empty log path must be refused (fail-closed)")
	} else if !errors.Is(err, selfeval.ErrChainBroken) {
		t.Errorf("empty-path error = %v, want it to wrap ErrChainBroken", err)
	}

	// Nonexistent path: eventlog.Verify errors, so the fold is refused.
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	if _, err := selfeval.Fold(context.Background(), missing, rep, led, nil); err == nil {
		t.Errorf("missing log path must be refused (no chain to verify)")
	}
	if len(led.Snapshot().Configs) != 0 {
		t.Errorf("a refused fold must leave the ledger untouched")
	}
}

// TestFoldHonorsContextCancellation proves the ctx-first contract: a cancelled
// context returns before any work.
func TestFoldHonorsContextCancellation(t *testing.T) {
	path := writeLog(t, []string{"task_start"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	led := trust.New()
	sink := &recordingSink{}
	rep := selfeval.NewVerifierJudged(verifierReport("native@gpt", 1, 1))

	if _, err := selfeval.Fold(ctx, path, rep, led, sink); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx error = %v, want context.Canceled", err)
	}
	if len(led.Snapshot().Configs) != 0 || len(sink.events) != 0 {
		t.Errorf("a cancelled fold must fold nothing and emit nothing")
	}
}
