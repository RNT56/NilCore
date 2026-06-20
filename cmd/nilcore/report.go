package main

// report.go implements `nilcore report` — the read-only verification-report UI
// (Phase 11, Pillar 6, P11-T33), the human face of invariant I2. It replays the
// append-only event log into a typed report.ReportModel (internal/report, a pure
// read that calls eventlog.Verify and refuses GREEN over a broken chain) and
// renders it to stdout via the pure renderers (internal/report/render): text by
// default (TTY-styled, plain on a pipe/CI), or self-contained script-free HTML /
// hand-rolled markdown. With -report-out it ALSO persists the rendered bytes to
// .nilcore/reports/<run>.<ext> through the confined worktree writer.
//
// Phase 12 (SW-T16) EXTENDS this ADDITIVELY with the swarm dimension: two new
// formats (`matrix` — the cross-shard claim grid render.RenderMatrix; `json` — the
// REDACTED projection render.MarshalRedacted, never a raw json.Marshal of the model
// so a key smuggled into a SourceURL can never ride out, I3) and an optional
// --dir <worktree> flag selecting the swarm worktree whose artifacts ReplaySwarmReport
// folds. The pre-Phase-12 path is untouched: text/md/html with NO --dir still go
// through ReplayReport exactly as before (byte-identical), so existing callers and
// the existing report tests stay green. The same renderers back BOTH the live swarm
// --report flag and this replay command, so live and replay share one code path.
//
// It mirrors the inspect/health dispatch: a new subcommand off main's switch, no
// new event kinds (purely a reader — I5), default subcommands byte-identical. The
// command logic lives in runReport so the exit-code/banner behavior is unit-
// testable without capturing os.Exit; reportMain only wires stdout + the exit.

import (
	"flag"
	"fmt"
	"os"

	"nilcore/internal/report"
	"nilcore/internal/report/render"
	"nilcore/internal/termui"
)

// reportMain is the `nilcore report` entrypoint. It builds the model, renders it
// to stdout in the requested format, optionally persists it under
// .nilcore/reports/, and exits non-zero on a broken chain (fail-closed) so the
// command doubles as a scripted trust gate. A genuinely unreadable log is a fatal
// error; a broken chain is NOT — it renders the RED banner and returns exit 1.
func reportMain(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	logPath := fs.String("log", "nilcore.events.jsonl", "append-only event log path")
	root := fs.String("root", ".", "worktree root holding .nilcore/artifacts (folded into the report)")
	reportOut := fs.String("report-out", "", "also write the rendered report under <root>/.nilcore/reports/<run>.<ext>")
	// --out is the SW-T16 alias of --report-out (the spec's documented name). When set
	// it takes precedence; --report-out stays accepted so the pre-Phase-12 invocation
	// is unchanged. Both route to the SAME confined report.WriteReport sink.
	out := fs.String("out", "", "alias of --report-out: also write the rendered report under .nilcore/reports/<run>.<ext>")
	format := fs.String("format", "text", "render format: text | md | html | json | matrix")
	// --dir selects the swarm worktree whose .nilcore/artifacts are folded via
	// ReplaySwarmReport (the swarm projection). Empty ⇒ the pre-Phase-12 single-run
	// path over --root. It is additive: omitting it preserves the original behavior.
	dir := fs.String("dir", "", "swarm worktree to fold via ReplaySwarmReport (empty ⇒ single-run replay over -root)")
	_ = fs.Parse(args)

	// An explicit <run> positional overrides the run name derived from the log path
	// (display metadata + the report-out filename); empty ⇒ the model's own runName.
	runOverride := ""
	if rest := fs.Args(); len(rest) > 0 {
		runOverride = rest[0]
	}

	// --out wins over --report-out when both are given; otherwise the original flag is
	// honored, so existing scripts keep working.
	persistOut := *reportOut
	if *out != "" {
		persistOut = *out
	}

	// Style is detected from the ACTUAL stdout writer (termui's isatty + NO_COLOR +
	// TERM check): a real terminal gets colour, a pipe/CI buffer gets plain bytes.
	st := termui.New(os.Stdout).Style()
	rendered, exit, err := runSwarmReport(*logPath, *root, *dir, *format, runOverride, persistOut, st)
	if err != nil {
		fatal(err)
	}
	fmt.Fprint(os.Stdout, rendered)
	if exit != 0 {
		os.Exit(exit)
	}
}

// runReport is the ORIGINAL single-run command core (P11-T33), preserved with its
// exact pre-Phase-12 signature so existing callers and the existing report tests
// compile and behave byte-identically. It is the no-swarm-dir case of runSwarmReport
// (dir=""), which for text/md/html takes the legacy ReplayReport path verbatim.
func runReport(logPath, root, format, runOverride, reportOut string, st termui.Style) (string, int, error) {
	return runSwarmReport(logPath, root, "", format, runOverride, reportOut, st)
}

// runSwarmReport is the pure command core (SW-T16, extending P11-T33), separated from
// reportMain so the exit code and the broken-chain banner are testable without
// os.Exit/stdout capture. It returns the rendered text to print, the process exit code
// (0 clean, 1 when the chain failed verification — fail-closed), and a fatal error for
// a genuinely unreadable log or an invalid format/output request.
//
// st only affects the text/matrix renderers (HTML/markdown/json are style-agnostic);
// a plain (non-styled) Style yields ANSI-free output so a redirected report stays
// clean.
//
// The model is built once and shared by every format. The swarm formats (matrix,
// json) and any --dir invocation fold the worktree's artifacts via ReplaySwarmReport
// (the swarm projection over Base); a plain text/md/html with NO --dir takes the
// original ReplayReport path so the pre-Phase-12 behavior is byte-identical. Either
// path runs eventlog.Verify (Base.ChainVerified), so a broken chain forces a RED
// banner AND exit 1 — never hidden, regardless of format (I2).
func runSwarmReport(logPath, root, dir, format, runOverride, reportOut string, st termui.Style) (string, int, error) {
	if err := validFormat(format); err != nil {
		return "", 0, err
	}

	// One model, shared by every renderer. The swarm formats and any --dir always
	// need the SwarmReport (RenderMatrix/MarshalRedacted consume it); the legacy
	// text/md/html-without-dir path stays on the original single-run ReplayReport so
	// existing callers see byte-identical output. Both reads run eventlog.Verify, so
	// FinalPass/ChainVerified govern the exit identically (I2).
	sr, m, err := buildReportModel(logPath, root, dir, format)
	if err != nil {
		return "", 0, err
	}
	if runOverride != "" {
		m.Run = runOverride
	}

	out := renderModel(sr, m, format, st)

	// Persist the rendered bytes to .nilcore/reports/<run>.<ext> through the confined
	// writer when asked. The writer enforces its own path safety (worktreefs); a bad
	// run/ext is surfaced as a fatal error rather than a silent skip. It writes under
	// the SAME root as the fold (--dir when given) so the report lands beside the
	// artifacts it describes.
	if reportOut != "" {
		if err := report.WriteReport(reportRoot(root, dir), m.Run, extFor(format), []byte(out)); err != nil {
			return "", 0, err
		}
	}

	// Fail-closed exit: a broken chain (eventlog.Verify failed) is not a trustworthy
	// report, so the command returns a non-zero code while STILL printing the RED
	// banner the renderer produced — the trust verdict is visible AND scriptable.
	exit := 0
	if !m.ChainVerified {
		exit = 1
	}
	return out, exit, nil
}

// buildReportModel replays the log into the model(s) the chosen format needs. The
// swarm formats (matrix/json) and ANY --dir invocation use ReplaySwarmReport — the
// authoritative read that reuses ReplayReport for Base (so the chain check + artifact
// fold + claim traces are computed once) and folds the swarm dimension on top. The
// legacy text/md/html path with NO --dir stays on ReplayReport alone, returning a nil
// SwarmReport, so the pre-Phase-12 output is byte-identical and no swarm fold cost is
// paid when it is not asked for. m (the Base/ReportModel) is always returned so the
// caller's run override + exit gate read from one place regardless of path.
func buildReportModel(logPath, root, dir, format string) (*report.SwarmReport, *report.ReportModel, error) {
	if dir != "" || needsSwarm(format) {
		sr, err := report.ReplaySwarmReport(logPath, foldRoot(root, dir))
		if err != nil {
			return nil, nil, err
		}
		return sr, sr.Base, nil
	}
	m, err := report.ReplayReport(logPath, root)
	if err != nil {
		return nil, nil, err
	}
	return nil, m, nil
}

// needsSwarm reports whether a format requires the SwarmReport projection (the
// cross-shard matrix or the redacted JSON deliverable). text/md/html render from the
// Base ReportModel alone.
func needsSwarm(format string) bool {
	return format == "matrix" || format == "json"
}

// foldRoot picks the worktree whose .nilcore/artifacts are folded: --dir when given
// (the swarm worktree), else --root. Keeping --dir distinct from --root lets a swarm
// run point the fold at a sibling worktree without disturbing the default.
func foldRoot(root, dir string) string {
	if dir != "" {
		return dir
	}
	return root
}

// reportRoot is the worktree the rendered report is persisted under with --report-out.
// It mirrors the fold root (foldRoot) so the report lands beside the artifacts it
// describes — under --dir when a swarm worktree was folded, else under --root.
func reportRoot(root, dir string) string {
	return foldRoot(root, dir)
}

// renderModel dispatches to the pure renderer for the chosen format. text/matrix are
// the style-aware formats; html/md/json ignore st by construction. matrix and json
// consume the SwarmReport (sr); the rest consume the Base ReportModel (m). sr is nil
// only on the legacy text/md/html path, which never reaches the swarm branches.
func renderModel(sr *report.SwarmReport, m *report.ReportModel, format string, st termui.Style) string {
	switch format {
	case "html":
		return render.RenderHTML(m)
	case "md":
		return render.RenderMarkdown(m)
	case "matrix":
		return render.RenderMatrix(sr, st)
	case "json":
		// The REDACTED projection, never a raw json.Marshal of the model: every
		// model-authored Value/SourceURL is scrubbed so a smuggled api_key=/token=
		// can never ride out in the deliverable (I3). A marshal error is treated as a
		// render failure and surfaced as the empty string (the format was validated up
		// front; MarshalRedacted only errors on a nil report, which cannot occur here).
		b, err := render.MarshalRedacted(sr)
		if err != nil {
			return ""
		}
		return string(b) + "\n"
	default: // "text"
		return render.RenderText(m, st)
	}
}

// validFormat rejects an unknown -format up front with an actionable error, so a
// typo fails loudly rather than silently rendering text.
func validFormat(format string) error {
	switch format {
	case "text", "html", "md", "json", "matrix":
		return nil
	default:
		return fmt.Errorf("report: unknown -format %q (want text | md | html | json | matrix)", format)
	}
}

// extFor maps a render format to the report file extension the worktree writer
// accepts (its closed {html,md,txt,json} allowlist): text/matrix ⇒ "txt" (the matrix
// is plain text), json ⇒ "json".
func extFor(format string) string {
	switch format {
	case "html":
		return "html"
	case "md":
		return "md"
	case "json":
		return "json"
	default: // text, matrix
		return "txt"
	}
}
