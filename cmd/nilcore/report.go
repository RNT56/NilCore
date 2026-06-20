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
	format := fs.String("format", "text", "render format: text | html | md")
	_ = fs.Parse(args)

	// An explicit <run> positional overrides the run name derived from the log path
	// (display metadata + the report-out filename); empty ⇒ the model's own runName.
	runOverride := ""
	if rest := fs.Args(); len(rest) > 0 {
		runOverride = rest[0]
	}

	// Style is detected from the ACTUAL stdout writer (termui's isatty + NO_COLOR +
	// TERM check): a real terminal gets colour, a pipe/CI buffer gets plain bytes.
	st := termui.New(os.Stdout).Style()
	out, exit, err := runReport(*logPath, *root, *format, runOverride, *reportOut, st)
	if err != nil {
		fatal(err)
	}
	fmt.Fprint(os.Stdout, out)
	if exit != 0 {
		os.Exit(exit)
	}
}

// runReport is the pure command core, separated from reportMain so the exit code
// and the broken-chain banner are testable without os.Exit/stdout capture. It
// returns the rendered text to print, the process exit code (0 clean, 1 when the
// chain failed verification — fail-closed), and a fatal error for a genuinely
// unreadable log or an invalid format/output request.
//
// st only affects the text renderer (HTML/markdown are style-agnostic); a plain
// (non-styled) Style yields ANSI-free text so a redirected report stays clean.
func runReport(logPath, root, format, runOverride, reportOut string, st termui.Style) (string, int, error) {
	if err := validFormat(format); err != nil {
		return "", 0, err
	}

	m, err := report.ReplayReport(logPath, root)
	if err != nil {
		return "", 0, err
	}
	if runOverride != "" {
		m.Run = runOverride
	}

	out := renderModel(m, format, st)

	// Persist the rendered bytes to .nilcore/reports/<run>.<ext> through the confined
	// writer when asked. The writer enforces its own path safety (worktreefs); a bad
	// run/ext is surfaced as a fatal error rather than a silent skip.
	if reportOut != "" {
		if err := report.WriteReport(root, m.Run, extFor(format), []byte(out)); err != nil {
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

// renderModel dispatches to the pure renderer for the chosen format. text is the
// only style-aware format; html/md ignore st by construction.
func renderModel(m *report.ReportModel, format string, st termui.Style) string {
	switch format {
	case "html":
		return render.RenderHTML(m)
	case "md":
		return render.RenderMarkdown(m)
	default: // "text"
		return render.RenderText(m, st)
	}
}

// validFormat rejects an unknown -format up front with an actionable error, so a
// typo fails loudly rather than silently rendering text.
func validFormat(format string) error {
	switch format {
	case "text", "html", "md":
		return nil
	default:
		return fmt.Errorf("report: unknown -format %q (want text | html | md)", format)
	}
}

// extFor maps a render format to the report file extension the worktree writer
// accepts (its closed {html,md,txt} allowlist): text ⇒ "txt".
func extFor(format string) string {
	switch format {
	case "html":
		return "html"
	case "md":
		return "md"
	default:
		return "txt"
	}
}
