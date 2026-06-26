// Package selfeval is the self-improvement flywheel's evidence-fold step (Phase 16,
// docs/ROADMAP-CLOSED-LOOP.md Pillar 4, SIF-T02). The flywheel periodically runs a
// content-hash-frozen eval suite against the agent itself; this package takes the
// resulting eval.Report and folds it back into the agent's earned standing — the
// per-config trust scoreboard (trust.Ledger.FoldEvalReport) and the experience
// projection (a "selfeval_report" event for later replay).
//
// It exists as its own leaf because the fold is the single most safety-sensitive
// step in the flywheel: it is the one place where the agent's own activity could,
// if unguarded, raise its routing/auto-approval standing. Two invariants therefore
// shape every line:
//
//   - I2 (the verifier is the SOLE authority on "done"). A report may only fold
//     VERIFIER-JUDGED outcomes — the per-case pass/fail an objective verifier
//     produced (eval.Runner returns the verifier's verdict, never a backend
//     self-report). A self-claim of success is structurally incapable of folding
//     here: VerifierJudged reports carry only the verifier verdict per case, and
//     Fold refuses any report not marked verifier-judged. The agent cannot inflate
//     its own standing.
//   - I5 (the event log is append-only and hash-chained). Folding is GATED behind
//     eventlog.Verify and FAILS CLOSED on a broken chain: a tampered or corrupt log
//     earns NOTHING (the fold is refused with an error and the ledger is left
//     untouched), exactly like trust.Replay (internal/trust/replay.go:78). A
//     tampered log can only WITHHOLD a fold, never forge one.
//
// This package never runs a model and opens no network (the harness invocation is
// a thin caller wired later, SIF-T08). It is default-off and nil-safe: a nil Sink
// emits no event and a nil Ledger folds nothing, so an unwired flywheel is
// byte-identical to today. It is a leaf — it imports eval, trust, eventlog, and
// experience (+ stdlib) and never the orchestrator (deps_test.go enforces this).
package selfeval

import (
	"context"
	"errors"
	"fmt"

	"nilcore/eval"
	"nilcore/internal/eventlog"
	"nilcore/internal/trust"
)

// EventKind is the append-only record this package writes for the experience
// projection to replay. It is metadata-only (config name, pass-rate, cost, case
// count, suite hash) — never raw model output (I7) and never a secret (I3).
const EventKind = "selfeval_report"

// ErrChainBroken is returned (wrapped) when the fold is refused because the event
// log's hash chain did not verify. Callers can match it with errors.Is to
// distinguish a fail-closed refusal from an ordinary error, but the safe action is
// the same in both cases: fold nothing.
var ErrChainBroken = errors.New("selfeval: event log chain broken, refusing to fold (fail-closed)")

// ErrNotVerifierJudged is returned when a report is presented for folding without
// the verifier-judged guarantee. A report that is not provably built from verifier
// verdicts is treated as a self-claim and refused — the agent must not raise its
// own standing from a self-generated, unjudged report (I2).
var ErrNotVerifierJudged = errors.New("selfeval: report is not verifier-judged, refusing to fold (I2)")

// Report wraps an eval.Report with the one guarantee this package will not infer:
// that every case verdict in it came from the objective verifier, not a backend
// self-report. Build it ONLY at a call site that ran the eval harness (eval.Run,
// whose Runner returns the verifier's pass/fail), via NewVerifierJudged. There is
// deliberately no way to mark an arbitrary eval.Report verifier-judged after the
// fact: the guarantee is established at the verifier boundary or not at all.
type Report struct {
	report         eval.Report
	verifierJudged bool
}

// NewVerifierJudged wraps a report produced by the eval harness, asserting at the
// verifier boundary that every Result.Passed in it is an objective verifier verdict
// (which eval.Run guarantees, because its Runner returns the verifier's pass/fail).
// This is the ONLY constructor that sets the verifier-judged flag, so a report that
// did not pass through the harness cannot be folded.
func NewVerifierJudged(r eval.Report) Report {
	return Report{report: r, verifierJudged: true}
}

// Passes counts the cases the VERIFIER passed in this report. It never reads a
// self-claim — eval.Result.Passed is the verifier's verdict by construction of the
// harness. Used by the fold and exposed for callers that want the raw count.
func (r Report) Passes() int {
	n := 0
	for _, res := range r.report.Results {
		if res.Passed {
			n++
		}
	}
	return n
}

// Cases is the number of scored cases in the report.
func (r Report) Cases() int { return len(r.report.Results) }

// Sink is the append-only event surface (satisfied by *eventlog.Log). It is the
// only write path out of this package; a nil Sink turns event emission off without
// affecting the trust fold. Defining it as an interface keeps the package a leaf
// (no eventlog.Log construction here) and lets tests record without a file.
type Sink interface {
	Append(e eventlog.Event)
}

// FoldResult reports what a fold did, for the caller's log/trace. It carries no
// secret and no model output, only the structural counts that were folded.
type FoldResult struct {
	Config   string  // the config the report scored
	Cases    int     // scored cases
	Passes   int     // verifier-passed cases
	PassRate float64 // report pass-rate (eval.Report.PassRate)
	ChainOK  bool    // whether the event-log chain verified (always true on success)
}

// Fold is the guarded fold: it verifies the event log's hash chain FIRST (I5,
// fail-closed) and refuses to fold a report that is not verifier-judged (I2), then
// folds the report into the per-config trust scoreboard (trust.Ledger.FoldEvalReport)
// and emits one append-only selfeval_report event (metadata only) for the
// experience projection.
//
// Order is deliberate and load-bearing:
//
//  1. Refuse a non-verifier-judged report (I2) — before touching any state.
//  2. Verify the chain (I5) — a broken chain refuses the fold with ErrChainBroken
//     and leaves the ledger untouched; the agent earns nothing over a tampered log.
//  3. Fold into trust and emit the record — only over a clean chain and a
//     verifier-judged report.
//
// Nil-safety / default-off: a nil ledger folds nothing (the chain is still verified
// and the event still emitted, so the audit trail is honest); a nil sink emits no
// event. An empty Config in the report folds nothing into trust (FoldEvalReport
// ignores it) but still verifies the chain and emits the record.
//
// logPath is the append-only event log to verify and is REQUIRED — folding without
// a chain to verify would defeat the I5 guard, so an empty logPath is an error.
func Fold(ctx context.Context, logPath string, r Report, ledger *trust.Ledger, sink Sink) (FoldResult, error) {
	if err := ctx.Err(); err != nil {
		return FoldResult{}, err
	}
	// (1) I2: a report that is not provably verifier-judged is a self-claim. Refuse
	// it before any state changes — the agent cannot fold a self-generated report.
	if !r.verifierJudged {
		return FoldResult{}, ErrNotVerifierJudged
	}
	if logPath == "" {
		return FoldResult{}, fmt.Errorf("selfeval: empty log path: %w", ErrChainBroken)
	}

	// (2) I5: gate the fold behind eventlog.Verify, fail-closed. A log we cannot
	// trust earns nothing — return ErrChainBroken and DO NOT touch the ledger.
	if err := eventlog.Verify(logPath); err != nil {
		return FoldResult{}, fmt.Errorf("%w: %v", ErrChainBroken, err)
	}

	res := FoldResult{
		Config:   r.report.Config,
		Cases:    r.Cases(),
		Passes:   r.Passes(),
		PassRate: r.report.PassRate,
		ChainOK:  true,
	}

	// (3) Fold into the per-config trust scoreboard. Reuse the existing, verifier-
	// judged fold (trust.Ledger.FoldEvalReport) so there is one fold law, not two; a
	// report with an empty Config is ignored there (nothing to attribute).
	if ledger != nil {
		ledger.FoldEvalReport(r.report)
	}

	// Emit the append-only projection record (I5: a normal Append, never a mutation;
	// I7: structural fields only, never raw model output; I3: no secret). The event
	// is emitted AFTER the chain verified so the audit trail reflects a real fold.
	if sink != nil {
		sink.Append(eventlog.Event{
			Kind:   EventKind,
			Detail: detailOf(res),
		})
	}
	return res, nil
}

// detailOf builds the metadata-only Detail map for the selfeval_report event. Every
// value is a structural count or the (operator-authored) config name — never raw
// verifier output, never a model emission, never a secret (I3/I7). It mirrors the
// shape eval reports already fold under so the experience projection can read it.
func detailOf(res FoldResult) map[string]any {
	return map[string]any{
		"config":    res.Config,
		"cases":     res.Cases,
		"passes":    res.Passes,
		"pass_rate": res.PassRate,
		"chain_ok":  res.ChainOK,
	}
}
