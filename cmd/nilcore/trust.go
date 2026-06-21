package main

// trust.go implements `nilcore trust` — the read-only Trust Ledger scoreboard
// (Phase 13), the operator face of strength routing. It replays the append-only
// event log into a trust.Ledger (internal/trust), folding every verifier-judged
// `race_outcome` into a per-backend pass-rate scoreboard, and (with --eval) an
// eval.Report into the per-config rollup, then renders it to stdout: a TTY-styled
// text table by default, or the snapshot as JSON.
//
// It mirrors inspect/report's read-only discipline: a new subcommand off main's
// switch, no new event kinds (purely a reader — I5), default behavior off the
// literal `trust` first-arg so the rest of the CLI is byte-identical. trust.Replay
// is FAIL-CLOSED on a broken hash chain: a tampered log yields NO trustworthy
// ranking, so the command prints the error and exits non-zero rather than ranking
// over forged evidence (I5). The ledger only ORDERS candidate backends; the
// verifier still decides "done" (I2) — Render says so in plain words.
//
// trustRouterFor builds the strength-routing Router for the orchestrator, but the
// single-backend run/serve construction sites cannot wire it correctly without a
// per-worktree backend seam the leaf does not expose (see the comment there); it
// is retained as the documented activation point and exercised by the test, while
// the live wiring stays deferred (out_of_scope_need).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"nilcore/eval"
	"nilcore/internal/backend"
	"nilcore/internal/termui"
	"nilcore/internal/trust"
)

// defaultLogPath is the standard append-only event-log path the read-only view
// commands (trust, trace/why) default to, matching how inspect/report resolve it.
const defaultLogPath = "nilcore.events.jsonl"

// trustMain is the `nilcore trust` entrypoint. It replays the event log into a
// ledger, optionally folds an eval report, and renders the scoreboard to stdout in
// the requested format. A broken hash chain (trust.Replay's fail-closed error) is
// printed and exits non-zero — a tampered log earns no ranking. Read-only: it never
// writes the log.
func trustMain(args []string) {
	fs := flag.NewFlagSet("trust", flag.ExitOnError)
	logPath := fs.String("log", defaultLogPath, "append-only event log path")
	format := fs.String("format", "text", "render format: text | json")
	evalPath := fs.String("eval", "", "optional eval report (JSON) to fold into the config scoreboard")
	_ = fs.Parse(args)

	out, err := runTrust(*logPath, *format, *evalPath, termui.New(os.Stdout).Style())
	if err != nil {
		// A broken chain surfaces here as trust.Replay's error: fail-closed, no
		// trustworthy ranking. Print it and exit non-zero so a script can detect it.
		fatal(err)
	}
	fmt.Fprint(os.Stdout, out)
}

// runTrust is the pure command core, separated from trustMain so the broken-chain
// behavior and the rendered output are unit-testable without os.Exit/stdout
// capture. It returns the rendered text and a fatal error (a broken chain or an
// unreadable log/eval is an error — the ranking is not trustworthy, so there is no
// half-output to print). st only affects the text renderer; JSON is style-agnostic.
func runTrust(logPath, format, evalPath string, st termui.Style) (string, error) {
	if err := validTrustFormat(format); err != nil {
		return "", err
	}

	// Replay is fail-closed: a broken chain returns an error and a nil ledger, so a
	// tampered log produces no ranking at all (I5). A missing log is a clean empty
	// ledger (no history yet), which renders the "defers to the default" line.
	ledger, err := trust.Replay(logPath)
	if err != nil {
		return "", fmt.Errorf("trust: %w", err)
	}

	// Optional eval fold: the report's own verifier-based pass rate + cost are folded
	// into the per-config scoreboard so the operator sees which config earned its
	// standing and at what cost. A bad/unreadable report is a hard error, not a silent
	// skip — the operator asked to fold it.
	if evalPath != "" {
		rep, rerr := readEvalReport(evalPath)
		if rerr != nil {
			return "", fmt.Errorf("trust: reading eval report: %w", rerr)
		}
		ledger.FoldEvalReport(rep)
	}

	snap := ledger.Snapshot()
	if format == "json" {
		b, merr := json.MarshalIndent(snap, "", "  ")
		if merr != nil {
			return "", fmt.Errorf("trust: marshalling snapshot: %w", merr)
		}
		return string(b) + "\n", nil
	}
	return trust.Render(snap, st), nil
}

// validTrustFormat rejects an unknown -format up front so a typo fails loudly
// rather than silently rendering text.
func validTrustFormat(format string) error {
	switch format {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("trust: unknown -format %q (want text | json)", format)
	}
}

// readEvalReport reads and decodes an eval.Report from a JSON file. It is a plain
// data read (the file is operator-supplied, not model-emitted), surfaced as an
// error on a missing file or malformed JSON.
func readEvalReport(path string) (eval.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return eval.Report{}, err
	}
	var rep eval.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return eval.Report{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return rep, nil
}

// trustRouterFor builds the strength-routing Router over the event log at logPath
// and a set of wired backends: it replays the ledger (fail-closed on a broken
// chain — no ranking over forged evidence, I5) and returns a *trust.Router that
// ORDERS candidate backends so the historically strongest WIRED one is tried first,
// else the configured default (I2 — it only selects; the verifier still decides
// "done"). A nil/empty ledger or a single-backend set degrades to returning the
// default unchanged (byte-identical to no router).
//
// It is the documented activation point for trust-routing in the orchestrator.
// Activation is gated on -trust / NILCORE_TRUST being set; UNSET leaves the
// SingleRouter default untouched, so the binary is byte-identical when trust is
// off. The run/serve construction sites build their backend per-worktree (NewEnv),
// so a Router holding a single construction-time instance would route to a backend
// pinned at the wrong worktree dir; correct live wiring needs a per-worktree seam
// the leaf does not expose, so the orchestrator activation is deferred (recorded as
// an out_of_scope_need). This helper is the ready, tested seam for that follow-on.
func trustRouterFor(logPath string, backends map[string]backend.CodingBackend, def backend.CodingBackend) (*trust.Router, error) {
	ledger, err := trust.Replay(logPath)
	if err != nil {
		return nil, fmt.Errorf("trust router: %w", err)
	}
	return trust.NewRouter(ledger, backends, def), nil
}

// trustEnabled reports whether strength routing is requested: the -trust flag (when
// a run/serve command threads it) or the NILCORE_TRUST env override. Default-off, so
// an unset environment keeps the SingleRouter default (byte-identical).
func trustEnabled(flagSet bool) bool {
	return flagSet || os.Getenv("NILCORE_TRUST") != ""
}

// routeContext is a tiny indirection so the test can exercise trustRouterFor's
// Route path without importing the orchestrator (the leaf rule applies to the
// helper too): it asks the Router to pick a backend for a throwaway task.
func routeContext(r *trust.Router, def backend.CodingBackend) backend.CodingBackend {
	return r.Route(context.Background(), backend.Task{ID: "trust-probe"}, def)
}
